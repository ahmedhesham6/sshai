package oci_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	oci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestClientPublishesAndPullsOnlyChangedLayer(t *testing.T) {
	t.Parallel()
	provider := newFileGrantProvider(t)
	client, err := oci.NewClient("owner-1", provider, oci.WithParallelism(3))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	first := buildTestCapsule(t, map[string]string{
		"config:editor": "editor = vim\n",
		"skill:review":  "review skill\n",
		"command:test":  "go test ./...\n",
	})
	if _, err := client.Publish(t.Context(), first); err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create destination store: %v", err)
	}
	gotFirst, err := client.Pull(t.Context(), first.Digest, destination, nil)
	if err != nil {
		t.Fatalf("Pull(first) error = %v", err)
	}
	if !reflect.DeepEqual(gotFirst, first) {
		t.Fatalf("first pull = %#v, want %#v", gotFirst, first)
	}

	present := make(map[string]struct{}, len(first.Layers))
	for _, layer := range first.Layers {
		present[layer.Digest] = struct{}{}
	}
	provider.resetRequests()
	second := buildTestCapsule(t, map[string]string{
		"config:editor": "editor = vim\n",
		"skill:review":  "review skill changed\n",
		"command:test":  "go test ./...\n",
	})
	if _, err := client.Publish(t.Context(), second); err != nil {
		t.Fatalf("Publish(second) error = %v", err)
	}
	gotSecond, err := client.Pull(t.Context(), second.Digest, destination, present)
	if err != nil {
		t.Fatalf("Pull(second) error = %v", err)
	}
	if !reflect.DeepEqual(gotSecond, second) {
		t.Fatalf("second pull = %#v, want %#v", gotSecond, second)
	}

	reads := provider.readKeys()
	for _, layer := range first.Layers {
		if layer.Digest != second.Layers[1].Digest && layer.Digest != second.Layers[2].Digest {
			continue
		}
		if layer.Digest == second.Layers[2].Digest {
			continue
		}
		if containsKey(reads, oci.BlobKey("owner-1", layer.Digest)) {
			t.Errorf("unchanged layer %s was fetched", layer.Digest)
		}
	}
	if got := countKey(reads, oci.BlobKey("owner-1", second.Layers[2].Digest)); got != 1 {
		t.Fatalf("changed layer fetches = %d, want 1; reads = %v", got, reads)
	}
}

func TestClientRejectsS3BlobDigestMismatch(t *testing.T) {
	t.Parallel()
	provider := newFileGrantProvider(t)
	client, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	capsuleValue := buildTestCapsule(t, map[string]string{"config:editor": "editor = vim\n"})
	if _, err := client.Publish(t.Context(), capsuleValue); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	provider.tamper(oci.BlobKey("owner-1", capsuleValue.Layers[0].Digest), []byte("wrong bytes"))
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create destination store: %v", err)
	}

	_, err = client.Pull(t.Context(), capsuleValue.Digest, destination, nil)
	if err == nil {
		t.Fatal("Pull() error = nil, want digest mismatch")
	}
	if !errors.Is(err, oci.ErrContentInvalid) {
		t.Fatalf("Pull() error = %v, want immutable content classification", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "digest") || !strings.Contains(err.Error(), capsuleValue.Layers[0].Digest) {
		t.Fatalf("Pull() error = %v, want expected digest mismatch", err)
	}
}

func TestClientRejectsTamperedIndexManifestDigest(t *testing.T) {
	t.Parallel()
	provider := newFileGrantProvider(t)
	client, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	first := buildTestCapsule(t, map[string]string{"config:editor": "first\n"})
	firstPublication, err := client.Publish(t.Context(), first)
	if err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}
	second := buildTestCapsule(t, map[string]string{"config:editor": "second\n"})
	secondPublication, err := client.Publish(t.Context(), second)
	if err != nil {
		t.Fatalf("Publish(second) error = %v", err)
	}

	manifestBytes := provider.object(t, oci.ManifestKey("owner-1", secondPublication.ManifestDigest))
	tamperedIndex, err := json.Marshal(ocispec.Index{
		Versioned: ocispec.Index{}.Versioned,
		Manifests: []ocispec.Descriptor{{
			MediaType:   ocispec.MediaTypeImageManifest,
			Digest:      digest.FromBytes(manifestBytes),
			Size:        int64(len(manifestBytes)),
			Annotations: map[string]string{ocispec.AnnotationRefName: first.Digest},
		}},
	})
	if err != nil {
		t.Fatalf("encode tampered index: %v", err)
	}
	provider.tamper(firstPublication.IndexKey, tamperedIndex)
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create destination store: %v", err)
	}

	_, err = client.Pull(t.Context(), first.Digest, destination, nil)
	if err == nil {
		t.Fatal("Pull() error = nil, want digest mismatch")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "digest mismatch") || !strings.Contains(err.Error(), secondPublication.ManifestDigest) {
		t.Fatalf("Pull() error = %v, want digest mismatch naming %s", err, secondPublication.ManifestDigest)
	}
	if containsKey(provider.readKeys(), oci.BlobKey("owner-1", second.Layers[0].Digest)) {
		t.Fatal("Pull() fetched a layer from the wrongly routed manifest")
	}
}

func TestClientRejectsOversizedRemoteObjectBeforeFullRead(t *testing.T) {
	t.Parallel()
	const indexLimit = 4 << 20
	provider := &boundedReadTestProvider{data: bytes.Repeat([]byte{'x'}, indexLimit+128)}
	client, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create destination store: %v", err)
	}

	_, err = client.Pull(t.Context(), "sha256:"+strings.Repeat("a", 64), destination, nil)
	if err == nil {
		t.Fatal("Pull() error = nil, want oversized index rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "index") || !strings.Contains(strings.ToLower(err.Error()), "maximum") {
		t.Fatalf("Pull() error = %v, want index maximum-size error", err)
	}
	if got, want := provider.bytesRead(), int64(indexLimit+1); got > want {
		t.Fatalf("remote bytes read = %d, want no more than %d", got, want)
	}
}

func TestClientRejectsGzipLayerBombAtDecompressionCap(t *testing.T) {
	provider := newFileGrantProvider(t)
	client, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	capsuleValue := buildTestCapsule(t, map[string]string{"config:editor": "editor\n"})
	const payloadSize = 64<<20 + 1
	layer := &capsuleValue.Layers[0]
	layer.Bytes = buildGzipBombLayer(t, layer.Index, payloadSize)
	layer.SizeBytes = int64(len(layer.Bytes))
	layer.Digest = digest.FromBytes(layer.Bytes).String()
	capsuleValue.Manifest.Components[0].Digest = layer.Digest
	capsuleValue.Manifest.Components[0].SizeBytes = layer.SizeBytes
	capsuleValue.Digest, err = capsule.ComputeCapsuleDigest(capsuleValue.Manifest)
	if err != nil {
		t.Fatalf("compute bomb Capsule digest: %v", err)
	}
	if _, err := client.Publish(t.Context(), capsuleValue); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create destination store: %v", err)
	}

	_, err = client.Pull(t.Context(), capsuleValue.Digest, destination, nil)
	if err == nil {
		t.Fatal("Pull() error = nil, want decompression limit rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "uncompressed") && !strings.Contains(strings.ToLower(err.Error()), "maximum") {
		t.Fatalf("Pull() error = %v, want decompression limit error", err)
	}
}

func TestClientStopsWorkersAndAvoidsUnverifiedLayerAfterFailure(t *testing.T) {
	provider := newFileGrantProvider(t)
	provider.blockedReads = make(map[string]struct{})
	client, err := oci.NewClient("owner-1", provider, oci.WithParallelism(5))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	capsuleValue := buildTestCapsule(t, map[string]string{
		"config:editor": "editor\n",
		"skill:review":  "review\n",
		"command:test":  "test\n",
		"hook:format":   "format\n",
		"template:one":  "template\n",
	})
	if _, err := client.Publish(t.Context(), capsuleValue); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	failedLayerKey := oci.BlobKey("owner-1", capsuleValue.Layers[0].Digest)
	provider.tamper(failedLayerKey, []byte("bad layer bytes"))
	for _, layer := range capsuleValue.Layers[1:] {
		provider.blockRead(oci.BlobKey("owner-1", layer.Digest))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
	defer cancel()
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create destination store: %v", err)
	}

	started := time.Now()
	_, err = client.Pull(ctx, capsuleValue.Digest, destination, nil)
	if err == nil {
		t.Fatal("Pull() error = nil, want failing layer error")
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("Pull() took %s, want workers canceled promptly", elapsed)
	}
	if got := provider.activeReads(); got != 0 {
		t.Fatalf("active remote reads after Pull() = %d, want 0", got)
	}
	layerDigest, err := digest.Parse(capsuleValue.Layers[0].Digest)
	if err != nil {
		t.Fatalf("parse failed layer digest: %v", err)
	}
	descriptor := ocispec.Descriptor{Digest: layerDigest, Size: capsuleValue.Layers[0].SizeBytes}
	exists, err := destination.Exists(t.Context(), descriptor)
	if err != nil {
		t.Fatalf("check failed layer in destination: %v", err)
	}
	if exists {
		t.Fatal("failed layer exists in destination, want no unverified blob")
	}
}

func TestClientRetriesExpiredReadGrantOnce(t *testing.T) {
	t.Parallel()
	provider := newFileGrantProvider(t)
	client, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	capsuleValue := buildTestCapsule(t, map[string]string{"config:editor": "editor\n"})
	publication, err := client.Publish(t.Context(), capsuleValue)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	provider.resetRequests()
	provider.expiredReadGrants = 1
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create destination store: %v", err)
	}

	got, err := client.Pull(t.Context(), capsuleValue.Digest, destination, nil)
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if !reflect.DeepEqual(got, capsuleValue) {
		t.Fatalf("pulled capsule = %#v, want %#v", got, capsuleValue)
	}
	if got := provider.grantCalls(publication.IndexKey); got != 2 {
		t.Fatalf("index grant calls = %d, want exactly 2", got)
	}
}

func buildGzipBombLayer(t *testing.T, entries []capsule.FileIndexEntry, payloadSize int64) []byte {
	t.Helper()
	indexJSON, err := (capsule.Layer{Index: entries}).CanonicalIndexJSON()
	if err != nil {
		t.Fatalf("encode bomb layer index: %v", err)
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: capsule.IndexPath, Mode: 0o644, Size: int64(len(indexJSON))}); err != nil {
		t.Fatalf("write bomb index header: %v", err)
	}
	if _, err := tarWriter.Write(indexJSON); err != nil {
		t.Fatalf("write bomb index: %v", err)
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "payload", Mode: 0o644, Size: payloadSize}); err != nil {
		t.Fatalf("write bomb payload header: %v", err)
	}
	if _, err := tarWriter.Write(bytes.Repeat([]byte{'x'}, int(payloadSize))); err != nil {
		t.Fatalf("write bomb payload: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close bomb tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close bomb gzip: %v", err)
	}
	return compressed.Bytes()
}

func TestClientPullsLayersConcurrentlyWithinConfiguredLimit(t *testing.T) {
	t.Parallel()
	provider := newFileGrantProvider(t)
	provider.readDelay = 20 * time.Millisecond
	client, err := oci.NewClient("owner-1", provider, oci.WithParallelism(3))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	capsuleValue := buildTestCapsule(t, map[string]string{
		"config:editor": "editor\n",
		"skill:review":  "review\n",
		"command:test":  "test\n",
		"hook:format":   "format\n",
	})
	if _, err := client.Publish(t.Context(), capsuleValue); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create destination store: %v", err)
	}
	got, err := client.Pull(t.Context(), capsuleValue.Digest, destination, nil)
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if !reflect.DeepEqual(got, capsuleValue) {
		t.Fatalf("pulled capsule = %#v, want %#v", got, capsuleValue)
	}
	if got := provider.maxActiveReads(); got < 2 {
		t.Fatalf("maximum concurrent reads = %d, want at least 2", got)
	}
	if got := provider.maxActiveReads(); got > 3 {
		t.Fatalf("maximum concurrent reads = %d, want no more than 3", got)
	}
}

func TestCapsuleReadKeysFetchesIndependentDigestsConcurrentlyInSortedOrder(t *testing.T) {
	provider := newFileGrantProvider(t)
	client, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatal(err)
	}
	digests := make([]string, 0, 3)
	for index, content := range []string{"alpha\n", "bravo\n", "charlie\n"} {
		value := buildTestCapsule(t, map[string]string{"config:editor": content})
		if _, err := client.Publish(t.Context(), value); err != nil {
			t.Fatalf("Publish(%d): %v", index, err)
		}
		digests = append(digests, value.Digest)
	}
	provider.resetRequests()
	provider.readDelay = 20 * time.Millisecond
	keys, err := oci.CapsuleReadKeys(t.Context(), "owner-1", []string{digests[2], digests[0], digests[1], digests[0]}, provider)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(keys) {
		t.Fatalf("Capsule read keys are not deterministic: %v", keys)
	}
	if got := provider.maxActiveReads(); got < 2 || got > 4 {
		t.Fatalf("maximum concurrent digest reads = %d, want 2..4", got)
	}
}

type fileGrantProvider struct {
	root              string
	mu                sync.Mutex
	requests          []oci.GrantRequest
	tampered          map[string][]byte
	readDelay         time.Duration
	blockedReads      map[string]struct{}
	expiredReadGrants int
	active            int
	maxActive         int
}

type boundedReadTestProvider struct {
	mu   sync.Mutex
	data []byte
	read int64
}

func (provider *boundedReadTestProvider) Grant(_ context.Context, _ oci.GrantRequest) (oci.Grant, error) {
	return oci.Grant{
		Read: func(readContext context.Context) (io.ReadCloser, error) {
			return &countingReadCloser{
				Reader: bytes.NewReader(provider.data),
				read: func(count int64) {
					provider.mu.Lock()
					provider.read += count
					provider.mu.Unlock()
				},
			}, nil
		},
	}, nil
}

func (provider *boundedReadTestProvider) bytesRead() int64 {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.read
}

type countingReadCloser struct {
	io.Reader
	read  func(int64)
	close func()
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.Reader.Read(buffer)
	reader.read(int64(count))
	return count, err
}

func (reader *countingReadCloser) Close() error {
	if reader.close != nil {
		reader.close()
	}
	return nil
}

func newFileGrantProvider(t *testing.T) *fileGrantProvider {
	t.Helper()
	return &fileGrantProvider{root: t.TempDir(), tampered: make(map[string][]byte)}
}

func (provider *fileGrantProvider) Grant(_ context.Context, request oci.GrantRequest) (oci.Grant, error) {
	provider.mu.Lock()
	provider.requests = append(provider.requests, request)
	expired := request.Operation == oci.GrantRead && provider.expiredReadGrants > 0
	if expired {
		provider.expiredReadGrants--
	}
	provider.mu.Unlock()
	grant := oci.Grant{
		Read: func(readContext context.Context) (io.ReadCloser, error) {
			provider.mu.Lock()
			provider.active++
			if provider.active > provider.maxActive {
				provider.maxActive = provider.active
			}
			delay := provider.readDelay
			tampered, hasTampered := provider.tampered[request.Key]
			provider.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if provider.isReadBlocked(request.Key) {
				select {
				case <-readContext.Done():
					provider.finishRead()
					return nil, readContext.Err()
				}
			}
			var data []byte
			var err error
			if hasTampered {
				data = append([]byte(nil), tampered...)
			} else {
				data, err = os.ReadFile(filepath.Join(provider.root, filepath.FromSlash(request.Key)))
			}
			if err != nil {
				provider.finishRead()
				return nil, err
			}
			return &trackedReadCloser{Reader: bytes.NewReader(data), close: provider.finishRead}, nil
		},
		Write: func(_ context.Context, reader io.Reader, _ int64) error {
			data, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			path := filepath.Join(provider.root, filepath.FromSlash(request.Key))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, data, 0o644)
		},
	}
	if expired {
		grant.ExpiresAt = time.Now().Add(-time.Minute)
	}
	return grant, nil
}

func (provider *fileGrantProvider) finishRead() {
	provider.mu.Lock()
	provider.active--
	provider.mu.Unlock()
}

func (provider *fileGrantProvider) tamper(key string, data []byte) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.tampered[key] = append([]byte(nil), data...)
}

func (provider *fileGrantProvider) blockRead(key string) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.blockedReads == nil {
		provider.blockedReads = make(map[string]struct{})
	}
	provider.blockedReads[key] = struct{}{}
}

func (provider *fileGrantProvider) isReadBlocked(key string) bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	_, blocked := provider.blockedReads[key]
	return blocked
}

func (provider *fileGrantProvider) activeReads() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.active
}

func (provider *fileGrantProvider) object(t *testing.T, key string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(provider.root, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("read provider object %s: %v", key, err)
	}
	return data
}

func (provider *fileGrantProvider) resetRequests() {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.requests = nil
	provider.maxActive = 0
}

func (provider *fileGrantProvider) readKeys() []string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	keys := make([]string, 0)
	for _, request := range provider.requests {
		if request.Operation == oci.GrantRead {
			keys = append(keys, request.Key)
		}
	}
	return keys
}

func (provider *fileGrantProvider) grantCalls(key string) int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	count := 0
	for _, request := range provider.requests {
		if request.Key == key && request.Operation == oci.GrantRead {
			count++
		}
	}
	return count
}

func (provider *fileGrantProvider) maxActiveReads() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.maxActive
}

type trackedReadCloser struct {
	*bytes.Reader
	close func()
}

func (reader *trackedReadCloser) Close() error {
	reader.close()
	return nil
}

func containsKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

func countKey(keys []string, want string) int {
	count := 0
	for _, key := range keys {
		if key == want {
			count++
		}
	}
	return count
}
