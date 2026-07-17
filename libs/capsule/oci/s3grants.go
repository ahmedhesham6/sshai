package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Presigner is the subset of *s3.PresignClient used by S3GrantProvider. It
// is satisfied by s3.NewPresignClient(s3.NewFromConfig(...)), the same
// construction apps/control-plane/cmd/control-plane/main.go's run() uses for
// the control plane's own capsule access presigner.
type S3Presigner interface {
	PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignPutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// S3GrantProvider is a production GrantProvider backed by presigned S3 URLs,
// modeled on apps/control-plane/capsule_contract.go's presignCapsuleObject
// and S3CapsuleOwnership: it presigns a GetObject/PutObject request scoped to
// the requested owner-prefixed key and, unlike the control plane (which
// hands the presigned URL to a browser), performs the HTTP round trip itself
// and returns the resulting reader/writer as a Grant.
//
// Rationale for a second, separate presigner in this package rather than
// reusing the control plane's POST /v1/capsule-access endpoint: by the
// ratified architecture (docs/spec/03-architecture.md), the Restate workflow
// service itself owns provider mutations and therefore holds AWS credentials
// directly — it is not a browser client of the control plane. The control
// plane's /v1/capsule-access endpoint authenticates callers as end users via
// WorkOS bearer tokens (see apps/control-plane's userFromContext /
// CreateCapsuleAccess), a credential the workflow service does not have and
// should not fabricate. Minting grants locally from the workflow service's
// own AWS credentials is therefore the only path consistent with that
// ownership split.
//
// This duplicates presign-and-validate logic already in
// apps/control-plane/capsule_contract.go. Deduplicating the two into a
// shared package is a reasonable follow-up once a second caller confirms the
// right shared shape; this slice does not refactor control-plane to avoid
// touching a component outside its scope.
type S3GrantProvider struct {
	presigner  S3Presigner
	bucket     string
	ttl        time.Duration
	httpClient *http.Client
}

// S3GrantProviderOption configures an S3GrantProvider.
type S3GrantProviderOption func(*S3GrantProvider)

// WithS3GrantHTTPClient overrides the HTTP client used to exercise minted
// presigned requests. The default is a client with a bounded timeout so a
// stalled upstream cannot hang a capsule sync indefinitely.
func WithS3GrantHTTPClient(client *http.Client) S3GrantProviderOption {
	return func(provider *S3GrantProvider) {
		if client != nil {
			provider.httpClient = client
		}
	}
}

// NewS3GrantProvider creates a GrantProvider that mints presigned S3 URLs
// scoped to the requesting owner's key prefix and are valid for ttl.
func NewS3GrantProvider(presigner S3Presigner, bucket string, ttl time.Duration, options ...S3GrantProviderOption) (*S3GrantProvider, error) {
	if presigner == nil {
		return nil, errors.New("create S3 grant provider: presigner is required")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, errors.New("create S3 grant provider: bucket is required")
	}
	if ttl <= 0 {
		return nil, errors.New("create S3 grant provider: TTL must be positive")
	}
	provider := &S3GrantProvider{
		presigner: presigner, bucket: bucket, ttl: ttl,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, option := range options {
		if option != nil {
			option(provider)
		}
	}
	return provider, nil
}

// Grant implements GrantProvider. It never mints a capability outside the
// owner-scoped prefix "owner/<OwnerID>/": every key is validated against that
// prefix before a presigned request is ever generated.
func (provider *S3GrantProvider) Grant(ctx context.Context, request GrantRequest) (Grant, error) {
	if provider == nil || provider.presigner == nil {
		return Grant{}, errors.New("mint Capsule grant: provider is not configured")
	}
	if err := validateGrantKey(request.OwnerID, request.Key); err != nil {
		return Grant{}, err
	}
	expiresAt := time.Now().UTC().Add(provider.ttl)
	switch request.Operation {
	case GrantRead:
		signed, err := provider.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(provider.bucket), Key: aws.String(request.Key),
		}, s3.WithPresignExpires(provider.ttl))
		if err != nil {
			return Grant{}, fmt.Errorf("mint Capsule read grant: %w", err)
		}
		client := provider.httpClient
		return Grant{
			ExpiresAt: expiresAt,
			Read: func(ctx context.Context) (io.ReadCloser, error) {
				return presignedRead(ctx, client, signed)
			},
		}, nil
	case GrantWrite:
		signed, err := provider.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(provider.bucket), Key: aws.String(request.Key),
		}, s3.WithPresignExpires(provider.ttl))
		if err != nil {
			return Grant{}, fmt.Errorf("mint Capsule write grant: %w", err)
		}
		client := provider.httpClient
		return Grant{
			ExpiresAt: expiresAt,
			Write: func(ctx context.Context, reader io.Reader, size int64) error {
				return presignedWrite(ctx, client, signed, reader, size)
			},
		}, nil
	default:
		return Grant{}, fmt.Errorf("mint Capsule grant: unsupported operation %q", request.Operation)
	}
}

func validateGrantKey(ownerID, key string) error {
	if strings.TrimSpace(ownerID) == "" {
		return errors.New("mint Capsule grant: owner ID is required")
	}
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return errors.New("mint Capsule grant: object key is invalid")
	}
	prefix := "owner/" + ownerID + "/"
	if !strings.HasPrefix(key, prefix) {
		return fmt.Errorf("mint Capsule grant: object key %q is outside owner %q's prefix", key, ownerID)
	}
	return nil
}

func presignedRead(ctx context.Context, client *http.Client, signed *v4.PresignedHTTPRequest) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, signed.Method, signed.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("read Capsule object: build request: %w", err)
	}
	copySignedHeader(request, signed.SignedHeader)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read Capsule object: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("read Capsule object: unexpected status %d %s: %s", response.StatusCode, response.Status, strings.TrimSpace(string(body)))
	}
	return response.Body, nil
}

func presignedWrite(ctx context.Context, client *http.Client, signed *v4.PresignedHTTPRequest, reader io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("write Capsule object: invalid negative size %d", size)
	}
	request, err := http.NewRequestWithContext(ctx, signed.Method, signed.URL, io.NopCloser(io.LimitReader(reader, size)))
	if err != nil {
		return fmt.Errorf("write Capsule object: build request: %w", err)
	}
	request.ContentLength = size
	copySignedHeader(request, signed.SignedHeader)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("write Capsule object: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("write Capsule object: unexpected status %d %s: %s", response.StatusCode, response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func copySignedHeader(request *http.Request, header http.Header) {
	for name, values := range header {
		if strings.EqualFold(name, "Host") {
			continue
		}
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
}

var _ GrantProvider = (*S3GrantProvider)(nil)
