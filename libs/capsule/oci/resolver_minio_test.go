package oci_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	oci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestMinIOCapsuleResolver exercises Resolver against a real S3-compatible
// (MinIO) backend, the same way TestMinIOCapsuleDistribution exercises
// Client. It is skipped when Docker is unavailable and is otherwise slow
// (container start-up); run it scoped, e.g.
// `go test ./libs/capsule/oci/... -run TestMinIOCapsuleResolver`.
func TestMinIOCapsuleResolver(t *testing.T) {
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
	bucket := "capsule-oci-resolver-test"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatalf("create MinIO bucket: %v", err)
	}
	provider := &minioGrantProvider{client: s3Client, bucket: bucket}
	ownerID := "owner-resolver"
	client, err := oci.NewClient(ownerID, provider)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	published := buildTestCapsule(t, map[string]string{
		"config:editor": "editor\n",
		"skill:review":  "review\n",
	})
	if _, err := client.Publish(ctx, published); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	resolver := oci.NewResolver(provider)

	t.Run("resolve by digest", func(t *testing.T) {
		ref := domain.CapsuleRef{
			Ref:             "owner/" + ownerID + "/capsule@" + published.Digest,
			FreshnessPolicy: domain.FreshnessPin,
		}
		resolution, err := resolver.Resolve(ctx, ownerID, ref)
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if resolution.OwnerID != ownerID {
			t.Fatalf("resolution owner = %q, want %q", resolution.OwnerID, ownerID)
		}
		if resolution.Digest != published.Digest {
			t.Fatalf("resolution digest = %q, want %q", resolution.Digest, published.Digest)
		}
		if len(resolution.Components) != len(published.Manifest.Components) {
			t.Fatalf("resolution components = %d, want %d", len(resolution.Components), len(published.Manifest.Components))
		}
		for index, component := range resolution.Components {
			want := published.Manifest.Components[index]
			if component.ID != want.ID || string(component.Type) != string(want.Type) || component.Digest != want.Digest {
				t.Fatalf("resolution component %d = %#v, want to match %#v", index, component, want)
			}
		}
	})

	t.Run("unknown digest is rejected", func(t *testing.T) {
		ref := domain.CapsuleRef{
			Ref:             "owner/" + ownerID + "/capsule@sha256:" + strings.Repeat("f", 64),
			FreshnessPolicy: domain.FreshnessPin,
		}
		if _, err := resolver.Resolve(ctx, ownerID, ref); err == nil {
			t.Fatal("Resolve() error = nil, want unknown digest error")
		}
	})
}
