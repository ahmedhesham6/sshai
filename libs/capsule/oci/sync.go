package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const (
	defaultParallelism  = 4
	maxIndexSize        = 4 << 20
	maxRemoteObjectSize = 256 << 20
)

var errExpiredGrant = errors.New("read grant is expired")

// GrantOperation identifies the object operation requested from the control
// plane's grant provider.
type GrantOperation string

const (
	// GrantRead requests a short-lived read capability.
	GrantRead GrantOperation = "read"
	// GrantWrite requests a short-lived write capability.
	GrantWrite GrantOperation = "write"
)

// GrantRequest identifies one owner-scoped object capability.
type GrantRequest struct {
	OwnerID   string
	Key       string
	Operation GrantOperation
}

// Grant is the capability returned by a GrantProvider. A presigned-URL
// provider can close over an HTTP request; a direct S3 provider can close over
// GetObject and PutObject calls. Only the function for the requested operation
// is required.
type Grant struct {
	// ExpiresAt is the capability expiry, when the provider knows it. A zero
	// value means the provider did not supply expiry metadata.
	ExpiresAt time.Time
	Read      func(context.Context) (io.ReadCloser, error)
	Write     func(context.Context, io.Reader, int64) error
}

// GrantProvider mints one short-lived capability for an owner-scoped object.
// The package never constructs credentials or broad bucket access itself.
type GrantProvider interface {
	Grant(context.Context, GrantRequest) (Grant, error)
}

// GrantProviderFunc adapts a function to GrantProvider.
type GrantProviderFunc func(context.Context, GrantRequest) (Grant, error)

// Grant implements GrantProvider.
func (fn GrantProviderFunc) Grant(ctx context.Context, request GrantRequest) (Grant, error) {
	return fn(ctx, request)
}

// Client syncs Capsule OCI layouts through an owner-scoped GrantProvider.
type Client struct {
	ownerID     string
	grants      GrantProvider
	parallelism int
}

// ClientOption configures a Client.
type ClientOption func(*Client) error

// WithParallelism bounds concurrent layer pulls. Values below one are
// rejected so an accidental zero cannot turn a cold pull into a deadlock.
func WithParallelism(parallelism int) ClientOption {
	return func(client *Client) error {
		if parallelism < 1 {
			return fmt.Errorf("parallelism must be positive: %d", parallelism)
		}
		client.parallelism = parallelism
		return nil
	}
}

// NewClient creates an owner-scoped Capsule sync client.
func NewClient(ownerID string, grants GrantProvider, options ...ClientOption) (*Client, error) {
	if err := validateOwnerID(ownerID); err != nil {
		return nil, err
	}
	if grants == nil {
		return nil, errors.New("create capsule OCI client: grant provider is required")
	}
	client := &Client{
		ownerID:     ownerID,
		grants:      grants,
		parallelism: min(defaultParallelism, max(1, runtime.GOMAXPROCS(0))),
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(client); err != nil {
			return nil, fmt.Errorf("create capsule OCI client: %w", err)
		}
	}
	return client, nil
}

// Publication identifies the immutable remote objects written by Publish.
type Publication struct {
	CapsuleDigest  string
	ManifestDigest string
	IndexKey       string
	BlobKeys       []string
}

// BlobKey returns the owner-scoped content-addressed key for an OCI blob.
func BlobKey(ownerID, digestString string) string {
	digestValue, err := digest.Parse(digestString)
	if err != nil {
		return ""
	}
	return path.Join("owner", ownerID, "blobs", digestValue.Algorithm().String(), digestValue.Encoded())
}

// IndexKey returns the content-addressed lookup key for a Capsule's OCI image
// layout index. The key is addressed by the Capsule manifest digest, while
// the stored bytes are the standard OCI index.json document.
func IndexKey(ownerID, capsuleDigest string) string {
	digestValue, err := digest.Parse(capsuleDigest)
	if err != nil {
		return ""
	}
	return path.Join("owner", ownerID, "index", "manifest", digestValue.Algorithm().String(), digestValue.Encoded())
}

// ManifestKey is an alias for the OCI image manifest's blob key. Image
// manifests are content-addressed blobs in an OCI image layout.
func ManifestKey(ownerID, manifestDigest string) string {
	return BlobKey(ownerID, manifestDigest)
}

// Publish assembles capsuleValue into an OCI image layout and uploads its
// index and all referenced blobs under the client's owner prefix.
func (client *Client) Publish(ctx context.Context, capsuleValue capsule.Capsule) (Publication, error) {
	if client == nil {
		return Publication{}, errors.New("publish capsule: client is nil")
	}
	layoutRoot, err := os.MkdirTemp("", "sshai-capsule-oci-")
	if err != nil {
		return Publication{}, fmt.Errorf("publish capsule: create temporary layout: %w", err)
	}
	defer os.RemoveAll(layoutRoot)
	store, err := orasoci.New(layoutRoot)
	if err != nil {
		return Publication{}, fmt.Errorf("publish capsule: create OCI layout: %w", err)
	}
	artifact, err := Assemble(ctx, store, capsuleValue)
	if err != nil {
		return Publication{}, err
	}

	descriptors := make([]ocispec.Descriptor, 0, len(artifact.LayerDescriptors)+2)
	descriptors = append(descriptors, artifact.ConfigDescriptor)
	descriptors = append(descriptors, artifact.LayerDescriptors...)
	descriptors = append(descriptors, artifact.ManifestDescriptor)
	seen := make(map[digest.Digest]struct{}, len(descriptors))
	blobKeys := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if _, exists := seen[descriptor.Digest]; exists {
			continue
		}
		seen[descriptor.Digest] = struct{}{}
		data, err := readLayoutBlob(layoutRoot, descriptor)
		if err != nil {
			return Publication{}, fmt.Errorf("publish capsule: read blob %s: %w", descriptor.Digest, err)
		}
		key := BlobKey(client.ownerID, descriptor.Digest.String())
		if err := client.writeObject(ctx, key, data); err != nil {
			return Publication{}, fmt.Errorf("publish capsule: upload blob %s: %w", descriptor.Digest, err)
		}
		blobKeys = append(blobKeys, key)
	}
	indexBytes, err := os.ReadFile(path.Join(layoutRoot, ocispec.ImageIndexFile))
	if err != nil {
		return Publication{}, fmt.Errorf("publish capsule: read OCI index: %w", err)
	}
	indexKey := IndexKey(client.ownerID, capsuleValue.Digest)
	if err := client.writeObject(ctx, indexKey, indexBytes); err != nil {
		return Publication{}, fmt.Errorf("publish capsule: upload OCI index: %w", err)
	}
	return Publication{
		CapsuleDigest:  capsuleValue.Digest,
		ManifestDigest: artifact.ManifestDescriptor.Digest.String(),
		IndexKey:       indexKey,
		BlobKeys:       blobKeys,
	}, nil
}

// Pull downloads targetDigest into destination and reconstructs the Capsule.
// Existing destination blobs and layer digests listed in presentLayerDigests
// are not fetched from the grant provider. Every downloaded object is checked
// against its expected digest before it is pushed into the destination.
func (client *Client) Pull(ctx context.Context, targetDigest string, destination *orasoci.Store, presentLayerDigests map[string]struct{}) (capsule.Capsule, error) {
	if client == nil {
		return capsule.Capsule{}, errors.New("pull capsule: client is nil")
	}
	if destination == nil {
		return capsule.Capsule{}, errors.New("pull capsule: destination OCI store is required")
	}
	target, err := parseSHA256Digest(targetDigest)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: target digest: %w", err)
	}
	indexBytes, err := client.readObject(ctx, IndexKey(client.ownerID, target.String()), maxIndexSize)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: fetch OCI index: %w", err)
	}
	manifestDescriptor, err := findManifestDescriptor(indexBytes, target.String())
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: resolve target %s: %w", target, err)
	}

	manifestBytes, err := client.fetchBlob(ctx, destination, manifestDescriptor)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: fetch image manifest: %w", err)
	}
	var imageManifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &imageManifest); err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: decode image manifest: %w", err)
	}
	if imageManifest.MediaType != ocispec.MediaTypeImageManifest || imageManifest.ArtifactType != capsule.ArtifactMediaType {
		return capsule.Capsule{}, errors.New("pull capsule: remote object is not a Capsule image manifest")
	}
	if imageManifest.Config.MediaType != ConfigMediaType {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: config media type %q is invalid", imageManifest.Config.MediaType)
	}
	configBytes, err := client.fetchBlob(ctx, destination, imageManifest.Config)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: fetch config: %w", err)
	}
	var capsuleManifest capsule.Manifest
	if err := json.Unmarshal(configBytes, &capsuleManifest); err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: decode config for target verification: %w", err)
	}
	computedCapsuleDigest, err := capsule.ComputeCapsuleDigest(capsuleManifest)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: compute target Capsule digest: %w", err)
	}
	if computedCapsuleDigest != target.String() {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: manifest %s digest mismatch: resolves Capsule digest %s, want %s", manifestDescriptor.Digest, computedCapsuleDigest, target)
	}
	if err := pushLocalBlob(ctx, destination, imageManifest.Config, configBytes); err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: store config: %w", err)
	}

	if err := client.pullLayers(ctx, destination, imageManifest.Layers, presentLayerDigests); err != nil {
		return capsule.Capsule{}, err
	}
	if err := pushLocalBlob(ctx, destination, manifestDescriptor, manifestBytes); err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: store image manifest: %w", err)
	}
	if err := destination.Tag(ctx, manifestDescriptor, target.String()); err != nil {
		return capsule.Capsule{}, fmt.Errorf("pull capsule: index image manifest: %w", err)
	}
	return Parse(ctx, destination, target.String())
}

func (client *Client) pullLayers(ctx context.Context, destination *orasoci.Store, descriptors []ocispec.Descriptor, present map[string]struct{}) error {
	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	group, groupContext := errgroup.WithContext(workerContext)
	semaphore := make(chan struct{}, client.parallelism)
	for _, descriptor := range descriptors {
		descriptor := descriptor
		if _, listed := present[descriptor.Digest.String()]; listed {
			exists, err := destination.Exists(groupContext, descriptor)
			if err != nil {
				return waitForPullWorkers(group, cancel, fmt.Errorf("pull capsule: check present layer %s: %w", descriptor.Digest, err))
			}
			if !exists {
				return waitForPullWorkers(group, cancel, fmt.Errorf("pull capsule: layer %s was marked present but is missing from destination", descriptor.Digest))
			}
			if _, err := fetchVerified(groupContext, destination, descriptor); err != nil {
				return waitForPullWorkers(group, cancel, fmt.Errorf("pull capsule: verify present layer %s: %w", descriptor.Digest, err))
			}
			continue
		}
		exists, err := destination.Exists(groupContext, descriptor)
		if err != nil {
			return waitForPullWorkers(group, cancel, fmt.Errorf("pull capsule: check layer %s: %w", descriptor.Digest, err))
		}
		if exists {
			if _, err := fetchVerified(groupContext, destination, descriptor); err != nil {
				return waitForPullWorkers(group, cancel, fmt.Errorf("pull capsule: verify existing layer %s: %w", descriptor.Digest, err))
			}
			continue
		}
		group.Go(func() error {
			select {
			case semaphore <- struct{}{}:
			case <-groupContext.Done():
				return groupContext.Err()
			}
			defer func() { <-semaphore }()
			data, err := client.readBlob(groupContext, descriptor)
			if err != nil {
				return fmt.Errorf("pull capsule: fetch layer %s: %w", descriptor.Digest, err)
			}
			if err := pushLocalBlob(groupContext, destination, descriptor, data); err != nil {
				return fmt.Errorf("pull capsule: store layer %s: %w", descriptor.Digest, err)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	return nil
}

func waitForPullWorkers(group *errgroup.Group, cancel context.CancelFunc, primary error) error {
	cancel()
	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return primary
}

func (client *Client) fetchBlob(ctx context.Context, destination *orasoci.Store, descriptor ocispec.Descriptor) ([]byte, error) {
	exists, err := destination.Exists(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	if exists {
		return fetchVerified(ctx, destination, descriptor)
	}
	return client.readBlob(ctx, descriptor)
}

func (client *Client) readBlob(ctx context.Context, descriptor ocispec.Descriptor) ([]byte, error) {
	return client.readObjectAsBlob(ctx, BlobKey(client.ownerID, descriptor.Digest.String()), descriptor)
}

func (client *Client) readObjectAsBlob(ctx context.Context, key string, descriptor ocispec.Descriptor) ([]byte, error) {
	if descriptor.Size < 0 {
		return nil, fmt.Errorf("blob %s has invalid negative size %d", key, descriptor.Size)
	}
	if descriptor.Size > maxRemoteObjectSize {
		return nil, fmt.Errorf("blob %s declared size %d exceeds maximum remote object size %d", key, descriptor.Size, maxRemoteObjectSize)
	}
	data, err := client.readObject(ctx, key, descriptor.Size)
	if err != nil {
		return nil, err
	}
	actual := digest.FromBytes(data)
	if actual != descriptor.Digest {
		return nil, fmt.Errorf("blob %s digest mismatch: got %s, want %s", key, actual, descriptor.Digest)
	}
	if int64(len(data)) != descriptor.Size {
		return nil, fmt.Errorf("blob %s size %d does not match expected %d", key, len(data), descriptor.Size)
	}
	return data, nil
}

func (client *Client) readObject(ctx context.Context, key string, maxSize int64) ([]byte, error) {
	if key == "" {
		return nil, errors.New("object key is invalid")
	}
	if maxSize < 0 {
		return nil, fmt.Errorf("object %s has invalid negative size limit %d", key, maxSize)
	}
	if maxSize > maxRemoteObjectSize {
		return nil, fmt.Errorf("object %s size limit %d exceeds maximum remote object size %d", key, maxSize, maxRemoteObjectSize)
	}
	data, err := client.readObjectOnce(ctx, key, maxSize)
	if !isExpiredGrantError(err) {
		return data, err
	}
	retryData, retryErr := client.readObjectOnce(ctx, key, maxSize)
	if retryErr != nil {
		return nil, fmt.Errorf("%w (fresh grant retry failed: %v)", err, retryErr)
	}
	return retryData, nil
}

func (client *Client) readObjectOnce(ctx context.Context, key string, maxSize int64) ([]byte, error) {
	grant, err := client.grants.Grant(ctx, GrantRequest{OwnerID: client.ownerID, Key: key, Operation: GrantRead})
	if err != nil {
		return nil, err
	}
	if !grant.ExpiresAt.IsZero() && !grant.ExpiresAt.After(time.Now()) {
		return nil, fmt.Errorf("%w at %s", errExpiredGrant, grant.ExpiresAt.Format(time.RFC3339Nano))
	}
	if grant.Read == nil {
		return nil, errors.New("read grant does not provide a reader")
	}
	reader, err := grant.Read(ctx)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxSize+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("object %s exceeds maximum size %d bytes", key, maxSize)
	}
	return data, nil
}

func isExpiredGrantError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errExpiredGrant) {
		return true
	}
	var statusError interface{ StatusCode() int }
	if errors.As(err, &statusError) && statusError.StatusCode() == 403 {
		return true
	}
	var httpStatusError interface{ HTTPStatusCode() int }
	if errors.As(err, &httpStatusError) && httpStatusError.HTTPStatusCode() == 403 {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "403") || strings.Contains(message, "forbidden")
}

func (client *Client) writeObject(ctx context.Context, key string, data []byte) error {
	if key == "" {
		return errors.New("object key is invalid")
	}
	grant, err := client.grants.Grant(ctx, GrantRequest{OwnerID: client.ownerID, Key: key, Operation: GrantWrite})
	if err != nil {
		return err
	}
	if grant.Write == nil {
		return errors.New("write grant does not provide a writer")
	}
	return grant.Write(ctx, bytes.NewReader(data), int64(len(data)))
}

func readLayoutBlob(root string, descriptor ocispec.Descriptor) ([]byte, error) {
	data, err := os.ReadFile(path.Join(root, ocispec.ImageBlobsDir, descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded()))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != descriptor.Size {
		return nil, fmt.Errorf("blob size %d does not match descriptor size %d", len(data), descriptor.Size)
	}
	if digest.FromBytes(data) != descriptor.Digest {
		return nil, fmt.Errorf("blob digest does not match descriptor %s", descriptor.Digest)
	}
	return data, nil
}

func pushLocalBlob(ctx context.Context, store *orasoci.Store, descriptor ocispec.Descriptor, data []byte) error {
	if int64(len(data)) != descriptor.Size {
		return fmt.Errorf("blob size %d does not match descriptor size %d", len(data), descriptor.Size)
	}
	if digest.FromBytes(data) != descriptor.Digest {
		return fmt.Errorf("blob digest mismatch: got %s, want %s", digest.FromBytes(data), descriptor.Digest)
	}
	exists, err := store.Exists(ctx, descriptor)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := store.Push(ctx, descriptor, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return nil
}

func findManifestDescriptor(indexBytes []byte, target string) (ocispec.Descriptor, error) {
	var index ocispec.Index
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("decode OCI index: %w", err)
	}
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations[ocispec.AnnotationRefName] == target {
			return descriptor, nil
		}
	}
	return ocispec.Descriptor{}, errors.New("Capsule digest is not present in OCI index")
}

func parseSHA256Digest(value string) (digest.Digest, error) {
	parsed, err := digest.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("algorithm %q is not sha256", parsed.Algorithm())
	}
	return parsed, nil
}

func validateOwnerID(ownerID string) error {
	if strings.TrimSpace(ownerID) == "" || ownerID == "." || ownerID == ".." || strings.ContainsAny(ownerID, "/\\") {
		return fmt.Errorf("create capsule OCI client: owner ID %q is invalid", ownerID)
	}
	return nil
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
