package provideraws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	metadataKind         = "sshai-kind"
	metadataDigest       = "sshai-digest"
	maxS3KeyBytes        = 1024
	maxSignedHeaderBytes = 8 << 10
)

type UploadConfig struct {
	Region      string
	Bucket      string
	EndpointURL string
}

type uploadS3API interface {
	GetBucketVersioning(context.Context, *s3.GetBucketVersioningInput, ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
	GetBucketEncryption(context.Context, *s3.GetBucketEncryptionInput, ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	CopyObject(context.Context, *s3.CopyObjectInput, ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
}

type uploadPresigner interface {
	PresignPutObject(context.Context, *s3.PutObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

type UploadStore struct {
	client    uploadS3API
	presigner uploadPresigner
	bucket    string
	now       func() time.Time
}

var (
	_ application.UploadSigner    = (*UploadStore)(nil)
	_ application.UploadInspector = (*UploadStore)(nil)
	_ application.UploadFinalizer = (*UploadStore)(nil)
)

func NewUploadStore(ctx context.Context, adapterConfig UploadConfig) (*UploadStore, error) {
	if strings.TrimSpace(adapterConfig.Region) == "" {
		return nil, errors.New("AWS upload store requires a region")
	}
	if err := validateUploadEndpoint(adapterConfig.EndpointURL); err != nil {
		return nil, err
	}
	sdkConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(adapterConfig.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS upload configuration: %w", err)
	}
	client := s3.NewFromConfig(sdkConfig, func(options *s3.Options) {
		if adapterConfig.EndpointURL != "" {
			options.BaseEndpoint = aws.String(adapterConfig.EndpointURL)
			options.UsePathStyle = true
		}
	})
	return newUploadStore(ctx, client, s3.NewPresignClient(client), adapterConfig, time.Now)
}

func newUploadStore(ctx context.Context, client uploadS3API, presigner uploadPresigner, adapterConfig UploadConfig, now func() time.Time) (*UploadStore, error) {
	bucket := strings.TrimSpace(adapterConfig.Bucket)
	if client == nil || presigner == nil || now == nil || bucket == "" {
		return nil, errors.New("AWS upload store requires an S3 client, presigner, bucket, and clock")
	}
	versioning, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return nil, containError("validate upload bucket versioning", err)
	}
	if versioning == nil || versioning.Status != types.BucketVersioningStatusEnabled {
		return nil, errors.New("AWS upload bucket versioning must be enabled")
	}
	encryption, err := client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: aws.String(bucket)})
	if err != nil {
		return nil, containError("validate upload bucket encryption", err)
	}
	if !hasDefaultEncryption(encryption) {
		return nil, errors.New("AWS upload bucket default encryption must be configured")
	}
	return &UploadStore{client: client, presigner: presigner, bucket: bucket, now: now}, nil
}

func (store *UploadStore) SignUpload(ctx context.Context, intent domain.UploadIntent) (application.SignedUpload, error) {
	snapshot := intent.Snapshot()
	if !validStagingKey(snapshot.ObjectKey, snapshot.Kind) || snapshot.SizeBytes < 0 {
		return application.SignedUpload{}, provider.NewError(provider.ErrorCodeInvalidRequest, "Upload Intent staging contract is invalid", nil)
	}
	checksum, err := digestChecksum(snapshot.Digest)
	if err != nil {
		return application.SignedUpload{}, provider.NewError(provider.ErrorCodeInvalidRequest, "Upload Intent digest is invalid", nil)
	}
	expires := snapshot.ExpiresAt.Sub(store.now().UTC().Truncate(time.Second))
	if expires <= 0 || expires > 7*24*time.Hour || expires%time.Second != 0 {
		return application.SignedUpload{}, provider.NewError(provider.ErrorCodeInvalidRequest, "Upload Intent expiry cannot be signed exactly", nil)
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(snapshot.ObjectKey), ContentLength: aws.Int64(snapshot.SizeBytes),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256, ChecksumSHA256: aws.String(checksum),
		Metadata: map[string]string{metadataKind: string(snapshot.Kind), metadataDigest: snapshot.Digest},
	}
	var request *v4.PresignedHTTPRequest
	for attempt := 0; attempt < 2; attempt++ {
		request, err = store.presigner.PresignPutObject(ctx, input, func(options *s3.PresignOptions) { options.Expires = expires })
		if err != nil {
			return application.SignedUpload{}, containError("sign upload", err)
		}
		signedAt, actualExpiry, valid := presignedExpiry(request)
		if valid && actualExpiry.Equal(snapshot.ExpiresAt) {
			break
		}
		expires = snapshot.ExpiresAt.Sub(signedAt)
		if !valid || expires <= 0 || expires > 7*24*time.Hour || expires%time.Second != 0 || attempt == 1 {
			return application.SignedUpload{}, provider.NewError(provider.ErrorCodeUnavailable, "sign upload could not bind the exact expiry", nil)
		}
	}
	if request == nil || request.Method != http.MethodPut || request.URL == "" {
		return application.SignedUpload{}, provider.NewError(provider.ErrorCodeUnavailable, "sign upload returned an invalid request", nil)
	}
	headers, err := requiredSignedHeaders(request.SignedHeader, map[string]string{
		"Content-Length":        strconv.FormatInt(snapshot.SizeBytes, 10),
		"X-Amz-Meta-Sshai-Kind": string(snapshot.Kind), "X-Amz-Meta-Sshai-Digest": snapshot.Digest,
	})
	if err != nil {
		return application.SignedUpload{}, provider.NewError(provider.ErrorCodeUnavailable, "sign upload returned an incomplete header contract", err)
	}
	if headers["X-Amz-Checksum-Sha256"] != checksum && !signedQueryValue(request.URL, "x-amz-checksum-sha256", checksum) {
		return application.SignedUpload{}, provider.NewError(provider.ErrorCodeUnavailable, "sign upload did not bind the checksum", nil)
	}
	return application.SignedUpload{URL: request.URL, RequiredHeaders: headers}, nil
}

func (store *UploadStore) InspectUpload(ctx context.Context, objectKey string) (application.ObservedUpload, error) {
	if !validS3Key(objectKey) || !strings.HasPrefix(objectKey, "uploads/") {
		return application.ObservedUpload{}, provider.NewError(provider.ErrorCodeInvalidRequest, "upload staging key is invalid", nil)
	}
	return store.inspectObject(ctx, objectKey, "")
}

func (store *UploadStore) FinalizeUpload(ctx context.Context, observed application.ObservedUpload, finalObjectKey string) error {
	if !validStagingKey(observed.ObjectKey, observed.Kind) || !validFinalKey(finalObjectKey) || observed.VersionID == "" || observed.SizeBytes < 0 {
		return provider.NewError(provider.ErrorCodeInvalidRequest, "upload finalization contract is invalid", nil)
	}
	if _, err := digestChecksum(observed.Digest); err != nil {
		return provider.NewError(provider.ErrorCodeInvalidRequest, "upload finalization digest is invalid", nil)
	}
	copySource := escapedCopySource(store.bucket, observed.ObjectKey, observed.VersionID)
	output, err := store.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(finalObjectKey), CopySource: aws.String(copySource),
		IfNoneMatch: aws.String("*"), MetadataDirective: types.MetadataDirectiveCopy,
	})
	if err != nil {
		if isPreconditionFailed(err) {
			existing, inspectErr := store.inspectObject(ctx, finalObjectKey, "")
			if inspectErr != nil {
				return inspectErr
			}
			if sameUpload(existing, observed, finalObjectKey) {
				return nil
			}
			return uploadDiverged("final upload object differs from verified content")
		}
		return containError("finalize upload", err)
	}
	if output == nil || aws.ToString(output.VersionId) == "" {
		return uploadDiverged("final upload object has no immutable version")
	}
	created, err := store.inspectObject(ctx, finalObjectKey, aws.ToString(output.VersionId))
	if err != nil {
		return err
	}
	if !sameUpload(created, observed, finalObjectKey) {
		return uploadDiverged("final upload object differs from verified content")
	}
	return nil
}

func (store *UploadStore) inspectObject(ctx context.Context, objectKey, versionID string) (application.ObservedUpload, error) {
	input := &s3.HeadObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(objectKey), ChecksumMode: types.ChecksumModeEnabled}
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}
	output, err := store.client.HeadObject(ctx, input)
	if err != nil {
		return application.ObservedUpload{}, containUploadError("inspect upload", err)
	}
	if output == nil {
		return application.ObservedUpload{}, uploadDiverged("upload object metadata is missing")
	}
	kind := domain.UploadKind(metadataValue(output.Metadata, metadataKind))
	digest := metadataValue(output.Metadata, metadataDigest)
	checksum, checksumErr := digestChecksum(digest)
	if !validUploadKind(kind) || checksumErr != nil || len(output.Metadata) != 2 || aws.ToString(output.ChecksumSHA256) != checksum || aws.ToString(output.VersionId) == "" || output.ContentLength == nil || aws.ToInt64(output.ContentLength) < 0 {
		return application.ObservedUpload{}, uploadDiverged("upload object metadata diverged")
	}
	if strings.HasPrefix(objectKey, "uploads/") && !validStagingKey(objectKey, kind) {
		return application.ObservedUpload{}, uploadDiverged("upload object kind diverged")
	}
	return application.ObservedUpload{
		ObjectKey: objectKey, Kind: kind, Digest: digest, SizeBytes: aws.ToInt64(output.ContentLength), VersionID: aws.ToString(output.VersionId),
	}, nil
}

func requiredSignedHeaders(signed http.Header, required map[string]string) (map[string]string, error) {
	if len(signed) > 16 {
		return nil, errors.New("too many signed headers")
	}
	headers := make(map[string]string, len(signed))
	total := 0
	for name, values := range signed {
		if len(values) != 1 || strings.ContainsAny(name+values[0], "\r\n") {
			return nil, errors.New("ambiguous signed header")
		}
		total += len(name) + len(values[0])
		if total > maxSignedHeaderBytes {
			return nil, errors.New("signed headers exceed bound")
		}
		headers[http.CanonicalHeaderKey(name)] = values[0]
	}
	for name, value := range required {
		if headers[http.CanonicalHeaderKey(name)] != value {
			return nil, fmt.Errorf("required header %q is not signed", name)
		}
	}
	return headers, nil
}

func signedQueryValue(rawURL, name, want string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	for key, values := range parsed.Query() {
		if strings.EqualFold(key, name) && len(values) == 1 && values[0] == want {
			return true
		}
	}
	return false
}

func presignedExpiry(request *v4.PresignedHTTPRequest) (time.Time, time.Time, bool) {
	if request == nil {
		return time.Time{}, time.Time{}, false
	}
	parsed, err := url.Parse(request.URL)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	var dateValue, expiresValue string
	for key, values := range parsed.Query() {
		if len(values) != 1 {
			continue
		}
		switch {
		case strings.EqualFold(key, "X-Amz-Date"):
			dateValue = values[0]
		case strings.EqualFold(key, "X-Amz-Expires"):
			expiresValue = values[0]
		}
	}
	signedAt, err := time.Parse("20060102T150405Z", dateValue)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	seconds, err := strconv.ParseInt(expiresValue, 10, 64)
	if err != nil || seconds <= 0 || seconds > int64((7*24*time.Hour)/time.Second) {
		return time.Time{}, time.Time{}, false
	}
	return signedAt, signedAt.Add(time.Duration(seconds) * time.Second), true
}

func digestChecksum(digest string) (string, error) {
	encoded := strings.TrimPrefix(digest, "sha256:")
	if len(encoded) != sha256.Size*2 || len(digest) != len("sha256:")+sha256.Size*2 {
		return "", errors.New("invalid digest")
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return "", errors.New("invalid digest")
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func hasDefaultEncryption(output *s3.GetBucketEncryptionOutput) bool {
	if output == nil || output.ServerSideEncryptionConfiguration == nil {
		return false
	}
	for _, rule := range output.ServerSideEncryptionConfiguration.Rules {
		if rule.ApplyServerSideEncryptionByDefault != nil {
			switch rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm {
			case types.ServerSideEncryptionAes256, types.ServerSideEncryptionAwsKms, types.ServerSideEncryptionAwsKmsDsse:
				return true
			}
		}
	}
	return false
}

func metadataValue(metadata map[string]string, key string) string {
	for candidate, value := range metadata {
		if strings.EqualFold(candidate, key) {
			return value
		}
	}
	return ""
}

func sameUpload(actual, expected application.ObservedUpload, actualKey string) bool {
	return actual.ObjectKey == actualKey && actual.Kind == expected.Kind && actual.Digest == expected.Digest && actual.SizeBytes == expected.SizeBytes && actual.VersionID != ""
}

func validStagingKey(key string, kind domain.UploadKind) bool {
	return validUploadKind(kind) && validS3Key(key) && strings.HasPrefix(key, "uploads/"+string(kind)+"/")
}

func validFinalKey(key string) bool {
	return validS3Key(key) && strings.HasPrefix(key, "objects/")
}

func validS3Key(key string) bool {
	return key == strings.TrimSpace(key) && key != "" && len(key) <= maxS3KeyBytes && !strings.ContainsAny(key, "\r\n")
}

func validUploadKind(kind domain.UploadKind) bool {
	switch kind {
	case domain.UploadProfileArtifact, domain.UploadGitBundle, domain.UploadTrackedPatch, domain.UploadUntrackedBundle, domain.UploadSeedManifest:
		return true
	default:
		return false
	}
}

func escapedCopySource(bucket, key, versionID string) string {
	path := (&url.URL{Path: "/" + bucket + "/" + key}).EscapedPath()
	return strings.TrimPrefix(path, "/") + "?versionId=" + url.QueryEscape(versionID)
}

func validateUploadEndpoint(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Path != "" {
		return errors.New("AWS upload endpoint is invalid")
	}
	if parsed.Scheme != "https" {
		return errors.New("AWS upload endpoint must use HTTPS")
	}
	return nil
}

func isPreconditionFailed(err error) bool {
	var apiError smithy.APIError
	if errors.As(err, &apiError) && apiError.ErrorCode() == "PreconditionFailed" {
		return true
	}
	var responseError interface{ HTTPStatusCode() int }
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == http.StatusPreconditionFailed
}

func containUploadError(operation string, err error) error {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchVersion":
			return fmt.Errorf("%s: %w", operation, application.ErrUploadObjectNotFound)
		}
	}
	var responseError interface{ HTTPStatusCode() int }
	if errors.As(err, &responseError) && responseError.HTTPStatusCode() == http.StatusNotFound {
		return fmt.Errorf("%s: %w", operation, application.ErrUploadObjectNotFound)
	}
	return containError(operation, err)
}

func uploadDiverged(message string) error {
	return provider.NewError(provider.ErrorCodeResourceDiverged, message, application.ErrUploadNotVerified)
}
