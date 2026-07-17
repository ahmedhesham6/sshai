package oci_test

import (
	"context"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	oci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestS3GrantProviderCapsuleDistribution(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("Docker is unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	container, err := testcontainers.Run(
		ctx,
		minioImage,
		testcontainers.WithEnv(map[string]string{
			"MINIO_ROOT_USER":     "minioadmin",
			"MINIO_ROOT_PASSWORD": "minioadmin",
		}),
		testcontainers.WithCmd("server", "/data"),
		testcontainers.WithExposedPorts("9000/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(90*time.Second,
			wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp")),
	)
	if err != nil {
		t.Skipf("Docker/MinIO unavailable: %v", err)
	}
	testcontainers.CleanupContainer(t, container)
	endpoint, err := container.PortEndpoint(ctx, "9000/tcp", "http")
	if err != nil {
		t.Fatalf("get MinIO endpoint: %v", err)
	}
	awsConfig, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", "")),
	)
	if err != nil {
		t.Fatalf("load MinIO AWS config: %v", err)
	}
	s3Client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})
	bucket := "capsule-oci-s3-grants-test"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatalf("create MinIO bucket: %v", err)
	}
	presigner := s3.NewPresignClient(s3Client)
	provider, err := oci.NewS3GrantProvider(presigner, bucket, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewS3GrantProvider() error = %v", err)
	}

	t.Run("publish capsule and pull by digest through presigned grants", func(t *testing.T) {
		client, err := oci.NewClient("owner-s3-grants", provider)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		value := buildTestCapsule(t, map[string]string{
			"config:editor": "editor\n",
			"skill:review":  "review\n",
		})
		if _, err := client.Publish(ctx, value); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		destination, err := orasoci.New(t.TempDir())
		if err != nil {
			t.Fatalf("create local destination: %v", err)
		}
		got, err := client.Pull(ctx, value.Digest, destination, nil)
		if err != nil {
			t.Fatalf("Pull() error = %v", err)
		}
		if !reflect.DeepEqual(got, value) {
			t.Fatalf("pulled capsule = %#v, want %#v", got, value)
		}
	})

	t.Run("reading a missing object surfaces the presigned GET's status", func(t *testing.T) {
		grant, err := provider.Grant(ctx, oci.GrantRequest{
			OwnerID: "owner-s3-grants", Key: "owner/owner-s3-grants/blobs/sha256/" + strings.Repeat("0", 64), Operation: oci.GrantRead,
		})
		if err != nil {
			t.Fatalf("Grant() error = %v", err)
		}
		if _, err := grant.Read(ctx); err == nil {
			t.Fatal("Read() error = nil, want a not-found error for a missing object")
		}
	})

	t.Run("write then read round-trips bytes through the presigned URLs", func(t *testing.T) {
		key := "owner/owner-s3-grants/blobs/sha256/" + strings.Repeat("1", 64)
		writeGrant, err := provider.Grant(ctx, oci.GrantRequest{OwnerID: "owner-s3-grants", Key: key, Operation: oci.GrantWrite})
		if err != nil {
			t.Fatalf("Grant(write) error = %v", err)
		}
		payload := []byte("hello capsule grant")
		if err := writeGrant.Write(ctx, strings.NewReader(string(payload)), int64(len(payload))); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		readGrant, err := provider.Grant(ctx, oci.GrantRequest{OwnerID: "owner-s3-grants", Key: key, Operation: oci.GrantRead})
		if err != nil {
			t.Fatalf("Grant(read) error = %v", err)
		}
		reader, err := readGrant.Read(ctx)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		defer reader.Close()
		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(got) != string(payload) {
			t.Fatalf("round-tripped body = %q, want %q", got, payload)
		}
	})
}

func TestS3GrantProviderRejectsKeysOutsideOwnerPrefix(t *testing.T) {
	ctx := context.Background()
	provider, err := oci.NewS3GrantProvider(stubPresigner{}, "bucket", time.Minute)
	if err != nil {
		t.Fatalf("NewS3GrantProvider() error = %v", err)
	}
	for _, testCase := range []struct {
		name    string
		request oci.GrantRequest
	}{
		{name: "different owner prefix", request: oci.GrantRequest{OwnerID: "owner-a", Key: "owner/owner-b/blobs/sha256/x", Operation: oci.GrantRead}},
		{name: "no owner prefix", request: oci.GrantRequest{OwnerID: "owner-a", Key: "blobs/sha256/x", Operation: oci.GrantRead}},
		{name: "empty key", request: oci.GrantRequest{OwnerID: "owner-a", Key: "", Operation: oci.GrantRead}},
		{name: "empty owner", request: oci.GrantRequest{OwnerID: "", Key: "owner/owner-a/blobs/sha256/x", Operation: oci.GrantRead}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := provider.Grant(ctx, testCase.request); err == nil {
				t.Fatal("Grant() error = nil, want a prefix-validation error")
			}
		})
	}
}

func TestNewS3GrantProviderValidatesConfiguration(t *testing.T) {
	if _, err := oci.NewS3GrantProvider(nil, "bucket", time.Minute); err == nil {
		t.Fatal("NewS3GrantProvider() error = nil, want error for nil presigner")
	}
	if _, err := oci.NewS3GrantProvider(stubPresigner{}, "", time.Minute); err == nil {
		t.Fatal("NewS3GrantProvider() error = nil, want error for empty bucket")
	}
	if _, err := oci.NewS3GrantProvider(stubPresigner{}, "bucket", 0); err == nil {
		t.Fatal("NewS3GrantProvider() error = nil, want error for non-positive TTL")
	}
}

type stubPresigner struct{}

func (stubPresigner) PresignGetObject(context.Context, *s3.GetObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return nil, nil
}

func (stubPresigner) PresignPutObject(context.Context, *s3.PutObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return nil, nil
}
