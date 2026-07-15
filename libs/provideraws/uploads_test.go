package provideraws

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestUploadStoreFailsClosedWithoutVersioningAndEncryption(t *testing.T) {
	encrypted := &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
		ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
	}}}
	for _, test := range []struct {
		name       string
		versioning types.BucketVersioningStatus
		encryption *types.ServerSideEncryptionConfiguration
	}{
		{name: "versioning disabled", encryption: encrypted},
		{name: "versioning suspended", versioning: types.BucketVersioningStatusSuspended, encryption: encrypted},
		{name: "encryption missing", versioning: types.BucketVersioningStatusEnabled},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &uploadS3Fake{versioning: test.versioning, encryption: test.encryption}
			if _, err := newUploadStore(t.Context(), client, &uploadPresignerFake{}, UploadConfig{Bucket: "uploads"}, time.Now); err == nil {
				t.Fatal("newUploadStore() error = nil")
			}
			if client.versioningCalls != 1 || (test.versioning == types.BucketVersioningStatusEnabled && client.encryptionCalls != 1) {
				t.Fatalf("validation calls = versioning:%d encryption:%d", client.versioningCalls, client.encryptionCalls)
			}
		})
	}
}

func TestUploadStoreRejectsInsecureProductionEndpoint(t *testing.T) {
	if _, err := NewUploadStore(t.Context(), UploadConfig{Region: "us-east-1", Bucket: "uploads", EndpointURL: "http://objects.example"}); err == nil {
		t.Fatal("NewUploadStore() error = nil")
	}
}

func TestUploadStoreSignsExactPutContractUntilIntentExpiry(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	intent := testUploadIntent(t, now, 12, "payload")
	presigner := &uploadPresignerFake{output: &v4.PresignedHTTPRequest{
		URL: "https://uploads.example/uploads/profile_artifact/staging?X-Amz-Date=20260713T070000Z&X-Amz-Expires=600&signature=ok", Method: http.MethodPut,
		SignedHeader: http.Header{
			"Content-Length":          {"12"},
			"X-Amz-Checksum-Sha256":   {testChecksum("payload")},
			"X-Amz-Meta-Sshai-Kind":   {string(domain.UploadProfileArtifact)},
			"X-Amz-Meta-Sshai-Digest": {testDigest("payload")},
		},
	}}
	store, err := newUploadStore(t.Context(), validUploadS3Fake(), presigner, UploadConfig{Bucket: "uploads"}, func() time.Time { return now.Add(987 * time.Millisecond) })
	if err != nil {
		t.Fatal(err)
	}

	signed, err := store.SignUpload(t.Context(), intent)
	if err != nil {
		t.Fatalf("SignUpload(): %v (cause: %v)", err, errors.Unwrap(err))
	}
	input := presigner.input
	if aws.ToString(input.Bucket) != "uploads" || aws.ToString(input.Key) != intent.Snapshot().ObjectKey || aws.ToInt64(input.ContentLength) != 12 || aws.ToString(input.ChecksumSHA256) != testChecksum("payload") {
		t.Fatalf("PutObject input = %#v", input)
	}
	if input.ChecksumAlgorithm != types.ChecksumAlgorithmSha256 || input.Metadata[metadataKind] != string(domain.UploadProfileArtifact) || input.Metadata[metadataDigest] != testDigest("payload") || len(input.Metadata) != 2 {
		t.Fatalf("PutObject checksum/metadata = %s %#v", input.ChecksumAlgorithm, input.Metadata)
	}
	if presigner.expires != 10*time.Minute {
		t.Fatalf("presign expiry = %s, want 10m", presigner.expires)
	}
	if signed.URL != presigner.output.URL || len(signed.RequiredHeaders) != 4 || signed.RequiredHeaders["Content-Length"] != "12" || signed.RequiredHeaders["X-Amz-Checksum-Sha256"] != testChecksum("payload") {
		t.Fatalf("SignedUpload = %#v", signed)
	}
}

func TestUploadStoreRejectsSignerExpiryDrift(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	intent := testUploadIntent(t, now, 12, "payload")
	presigner := &uploadPresignerFake{output: &v4.PresignedHTTPRequest{
		URL:    "https://uploads.example/upload?X-Amz-Date=20260713T070001Z&X-Amz-Expires=600",
		Method: http.MethodPut,
		SignedHeader: http.Header{
			"Content-Length": {"12"}, "X-Amz-Checksum-Sha256": {testChecksum("payload")},
			"X-Amz-Meta-Sshai-Kind": {string(domain.UploadProfileArtifact)}, "X-Amz-Meta-Sshai-Digest": {testDigest("payload")},
		},
	}}
	store, err := newUploadStore(t.Context(), validUploadS3Fake(), presigner, UploadConfig{Bucket: "uploads"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SignUpload(t.Context(), intent); err == nil || presigner.calls != 2 {
		t.Fatalf("SignUpload() = calls:%d error:%v", presigner.calls, err)
	}
}

func TestUploadStoreInspectsChecksumMetadataAndImmutableVersion(t *testing.T) {
	client := validUploadS3Fake()
	client.headOutput = &s3.HeadObjectOutput{
		ChecksumSHA256: aws.String(testChecksum("payload")), ContentLength: aws.Int64(12), VersionId: aws.String("version-1"),
		Metadata: map[string]string{metadataKind: string(domain.UploadProfileArtifact), metadataDigest: testDigest("payload")},
	}
	store, err := newUploadStore(t.Context(), client, &uploadPresignerFake{}, UploadConfig{Bucket: "uploads"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := store.InspectUpload(t.Context(), "uploads/profile_artifact/staging")
	if err != nil {
		t.Fatalf("InspectUpload(): %v", err)
	}
	if client.headInput.ChecksumMode != types.ChecksumModeEnabled || client.headInput.VersionId != nil {
		t.Fatalf("HeadObject input = %#v", client.headInput)
	}
	if observed.ObjectKey != "uploads/profile_artifact/staging" || observed.Kind != domain.UploadProfileArtifact || observed.Digest != testDigest("payload") || observed.SizeBytes != 12 || observed.VersionID != "version-1" {
		t.Fatalf("ObservedUpload = %#v", observed)
	}
}

func TestUploadStoreFinalizesExactSourceVersionWithoutReplacement(t *testing.T) {
	client := validUploadS3Fake()
	client.copyOutput = &s3.CopyObjectOutput{VersionId: aws.String("final-version-1")}
	client.headOutput = &s3.HeadObjectOutput{
		ChecksumSHA256: aws.String(testChecksum("payload")), ContentLength: aws.Int64(12), VersionId: aws.String("final-version-1"),
		Metadata: map[string]string{metadataKind: string(domain.UploadProfileArtifact), metadataDigest: testDigest("payload")},
	}
	store, err := newUploadStore(t.Context(), client, &uploadPresignerFake{}, UploadConfig{Bucket: "uploads"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	observed := application.ObservedUpload{
		ObjectKey: "uploads/profile_artifact/staging", Kind: domain.UploadProfileArtifact,
		Digest: testDigest("payload"), SizeBytes: 12, VersionID: "source-version-7",
	}
	if err := store.FinalizeUpload(t.Context(), observed, "objects/owner/profile_artifact/digest"); err != nil {
		t.Fatalf("FinalizeUpload(): %v", err)
	}
	wantSource := "uploads/uploads/profile_artifact/staging?versionId=" + url.QueryEscape("source-version-7")
	if aws.ToString(client.copyInput.CopySource) != wantSource || aws.ToString(client.copyInput.Key) != "objects/owner/profile_artifact/digest" || aws.ToString(client.copyInput.IfNoneMatch) != "*" || client.copyInput.MetadataDirective != types.MetadataDirectiveCopy {
		t.Fatalf("CopyObject input = %#v", client.copyInput)
	}
	if aws.ToString(client.headInput.VersionId) != "final-version-1" || client.headInput.ChecksumMode != types.ChecksumModeEnabled {
		t.Fatalf("final HeadObject input = %#v", client.headInput)
	}
}

func TestUploadStoreFinalizationConvergesOnlyForMatchingExistingContent(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata map[string]string
		checksum string
		size     int64
		wantErr  bool
	}{
		{
			name: "same content", checksum: testChecksum("payload"), size: 12,
			metadata: map[string]string{metadataKind: string(domain.UploadProfileArtifact), metadataDigest: testDigest("payload")},
		},
		{
			name: "different digest", checksum: testChecksum("other"), size: 12, wantErr: true,
			metadata: map[string]string{metadataKind: string(domain.UploadProfileArtifact), metadataDigest: testDigest("other")},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := validUploadS3Fake()
			client.copyErr = &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "exists"}
			client.headOutput = &s3.HeadObjectOutput{
				ChecksumSHA256: aws.String(test.checksum), ContentLength: aws.Int64(test.size), VersionId: aws.String("existing-version"), Metadata: test.metadata,
			}
			store, err := newUploadStore(t.Context(), client, &uploadPresignerFake{}, UploadConfig{Bucket: "uploads"}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			err = store.FinalizeUpload(t.Context(), application.ObservedUpload{
				ObjectKey: "uploads/profile_artifact/staging", Kind: domain.UploadProfileArtifact,
				Digest: testDigest("payload"), SizeBytes: 12, VersionID: "source-version",
			}, "objects/owner/profile_artifact/digest")
			if (err != nil) != test.wantErr {
				t.Fatalf("FinalizeUpload() error = %v, want error %t", err, test.wantErr)
			}
			if test.wantErr && !errors.Is(err, application.ErrUploadNotVerified) {
				t.Fatalf("FinalizeUpload() error = %v, want ErrUploadNotVerified", err)
			}
		})
	}
}

func TestUploadStoreMiniStackPresignedPutPersistsMetadataAndVersion(t *testing.T) {
	setMiniStackCredentials(t)
	container, err := testcontainers.Run(t.Context(), miniStackImage,
		testcontainers.WithExposedPorts("4566/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/_ministack/ready").WithPort("4566/tcp")),
	)
	if err != nil {
		t.Fatalf("start MiniStack: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate MiniStack: %v", err)
		}
	})
	endpoint, err := container.Endpoint(t.Context(), "http")
	if err != nil {
		t.Fatal(err)
	}
	sdkConfig, err := config.LoadDefaultConfig(t.Context(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(sdkConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})
	const bucket = "sshai-upload-tests"
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket(): %v", err)
	}
	if _, err := client.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning(): %v", err)
	}
	if _, err := client.PutBucketEncryption(t.Context(), &s3.PutBucketEncryptionInput{
		Bucket: aws.String(bucket), ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
		}}},
	}); err != nil {
		t.Fatalf("PutBucketEncryption(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	store, err := newUploadStore(t.Context(), client, s3.NewPresignClient(client), UploadConfig{Bucket: bucket}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newUploadStore(): %v", err)
	}
	payload := []byte("payload-data")
	intent := testUploadIntent(t, now, int64(len(payload)), string(payload))
	signed, err := store.SignUpload(t.Context(), intent)
	if err != nil {
		t.Fatalf("SignUpload(): %v (cause: %v)", err, errors.Unwrap(err))
	}
	parsedURL, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatal(err)
	}
	signedAt, err := time.Parse("20060102T150405Z", parsedURL.Query().Get("X-Amz-Date"))
	if err != nil {
		t.Fatalf("parse signing time: %v", err)
	}
	expiresSeconds, err := strconv.ParseInt(parsedURL.Query().Get("X-Amz-Expires"), 10, 64)
	if err != nil {
		t.Fatalf("parse signing expiry: %v", err)
	}
	if got := signedAt.Add(time.Duration(expiresSeconds) * time.Second); !got.Equal(intent.Snapshot().ExpiresAt) {
		t.Fatalf("presigned request expires at %s, want %s", got, intent.Snapshot().ExpiresAt)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, signed.URL, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range signed.RequiredHeaders {
		if name == "Content-Length" {
			request.ContentLength, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("presigned PUT: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT status = %s", response.Status)
	}
	// MiniStack 1.3.14 does not return the uploaded SHA-256 from HeadObject with
	// checksum mode enabled. Exact checksum inspection and version-pinned copy are
	// therefore covered by the typed request-capture tests above; the emulator owns
	// the supported presigned PUT, metadata, size, and bucket-versioning behavior.
	head, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(intent.Snapshot().ObjectKey)})
	if err != nil {
		t.Fatalf("HeadObject(): %v", err)
	}
	if aws.ToString(head.VersionId) == "" || aws.ToInt64(head.ContentLength) != int64(len(payload)) || metadataValue(head.Metadata, metadataKind) != string(domain.UploadProfileArtifact) || metadataValue(head.Metadata, metadataDigest) != intent.Snapshot().Digest {
		t.Fatalf("HeadObject output = %#v", head)
	}
}

func testUploadIntent(t *testing.T, now time.Time, size int64, payload string) domain.UploadIntent {
	t.Helper()
	intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-1", OwnerUserID: "user-1", Kind: domain.UploadProfileArtifact,
		Digest: testDigest(payload), SizeBytes: size, ObjectKey: "uploads/profile_artifact/staging",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func testDigest(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("sha256:%x", sum)
}

func testChecksum(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func validUploadS3Fake() *uploadS3Fake {
	return &uploadS3Fake{
		versioning: types.BucketVersioningStatusEnabled,
		encryption: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
		}}},
	}
}

type uploadPresignerFake struct {
	input   *s3.PutObjectInput
	expires time.Duration
	calls   int
	output  *v4.PresignedHTTPRequest
	err     error
}

func (fake *uploadPresignerFake) PresignPutObject(_ context.Context, input *s3.PutObjectInput, options ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	fake.calls++
	fake.input = input
	configured := &s3.PresignOptions{}
	for _, option := range options {
		option(configured)
	}
	fake.expires = configured.Expires
	return fake.output, fake.err
}

type uploadS3Fake struct {
	versioningCalls, encryptionCalls int
	versioning                       types.BucketVersioningStatus
	encryption                       *types.ServerSideEncryptionConfiguration
	headInput                        *s3.HeadObjectInput
	headOutput                       *s3.HeadObjectOutput
	headErr                          error
	copyInput                        *s3.CopyObjectInput
	copyOutput                       *s3.CopyObjectOutput
	copyErr                          error
}

func (fake *uploadS3Fake) GetBucketVersioning(context.Context, *s3.GetBucketVersioningInput, ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	fake.versioningCalls++
	return &s3.GetBucketVersioningOutput{Status: fake.versioning}, nil
}

func (fake *uploadS3Fake) GetBucketEncryption(context.Context, *s3.GetBucketEncryptionInput, ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error) {
	fake.encryptionCalls++
	return &s3.GetBucketEncryptionOutput{ServerSideEncryptionConfiguration: fake.encryption}, nil
}

func (fake *uploadS3Fake) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	fake.headInput = input
	return fake.headOutput, fake.headErr
}

func (fake *uploadS3Fake) CopyObject(_ context.Context, input *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	fake.copyInput = input
	return fake.copyOutput, fake.copyErr
}
