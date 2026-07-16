package oci_test

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	oci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	orasoci "oras.land/oras-go/v2/content/oci"
)

const minioImage = "minio/minio:RELEASE.2024-06-13T22-53-53Z"

func TestMinIOCapsuleDistribution(t *testing.T) {
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
	bucket := "capsule-oci-test"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatalf("create MinIO bucket: %v", err)
	}
	provider := &minioGrantProvider{client: s3Client, bucket: bucket}
	client, err := oci.NewClient("owner-minio", provider, oci.WithParallelism(3))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	t.Run("publish capsule and pull by digest", func(t *testing.T) {
		value := buildTestCapsule(t, map[string]string{
			"config:editor": "editor\n",
			"skill:review":  "review\n",
		})
		if _, err := client.Publish(ctx, value); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		destination, err := newMinIOStore(t)
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

	t.Run("second pull fetches changed layer only", func(t *testing.T) {
		first := buildTestCapsule(t, map[string]string{
			"config:editor": "editor\n",
			"skill:review":  "review\n",
			"command:test":  "test\n",
		})
		if _, err := client.Publish(ctx, first); err != nil {
			t.Fatalf("Publish(first) error = %v", err)
		}
		destination, err := newMinIOStore(t)
		if err != nil {
			t.Fatalf("create local destination: %v", err)
		}
		if _, err := client.Pull(ctx, first.Digest, destination, nil); err != nil {
			t.Fatalf("Pull(first) error = %v", err)
		}
		present := make(map[string]struct{}, len(first.Layers))
		for _, layer := range first.Layers {
			present[layer.Digest] = struct{}{}
		}
		second := buildTestCapsule(t, map[string]string{
			"config:editor": "editor\n",
			"skill:review":  "review changed\n",
			"command:test":  "test\n",
		})
		if _, err := client.Publish(ctx, second); err != nil {
			t.Fatalf("Publish(second) error = %v", err)
		}
		provider.resetReads()
		if _, err := client.Pull(ctx, second.Digest, destination, present); err != nil {
			t.Fatalf("Pull(second) error = %v", err)
		}
		reads := provider.readKeys()
		for _, layer := range first.Layers {
			if layer.Digest == second.Layers[2].Digest {
				continue
			}
			if containsKey(reads, oci.BlobKey("owner-minio", layer.Digest)) {
				t.Errorf("unchanged layer %s was fetched", layer.Digest)
			}
		}
		if got := countKey(reads, oci.BlobKey("owner-minio", second.Layers[2].Digest)); got != 1 {
			t.Fatalf("changed layer fetches = %d, want 1; reads = %v", got, reads)
		}
	})

	t.Run("tampered bucket blob is rejected by digest", func(t *testing.T) {
		value := buildTestCapsule(t, map[string]string{"config:editor": "editor\n"})
		if _, err := client.Publish(ctx, value); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		key := oci.BlobKey("owner-minio", value.Layers[0].Digest)
		badBytes := []byte("tampered blob bytes")
		badSize := int64(len(badBytes))
		if _, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        &bucket,
			Key:           &key,
			Body:          bytes.NewReader(badBytes),
			ContentLength: &badSize,
		}); err != nil {
			t.Fatalf("tamper bucket blob: %v", err)
		}
		destination, err := newMinIOStore(t)
		if err != nil {
			t.Fatalf("create local destination: %v", err)
		}

		_, err = client.Pull(ctx, value.Digest, destination, nil)
		if err == nil {
			t.Fatal("Pull() error = nil, want digest mismatch")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "digest") || !strings.Contains(err.Error(), value.Layers[0].Digest) {
			t.Fatalf("Pull() error = %v, want digest mismatch for %s", err, value.Layers[0].Digest)
		}
	})

	t.Run("concurrent pull round-trips correctly", func(t *testing.T) {
		value := buildTestCapsule(t, map[string]string{
			"config:editor": "editor\n",
			"skill:review":  "review\n",
			"command:test":  "test\n",
			"hook:format":   "format\n",
		})
		if _, err := client.Publish(ctx, value); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		destination, err := newMinIOStore(t)
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
}

func newMinIOStore(t *testing.T) (*orasoci.Store, error) {
	t.Helper()
	return orasoci.New(t.TempDir())
}

type minioGrantProvider struct {
	client *s3.Client
	bucket string
	mu     sync.Mutex
	reads  []string
}

func (provider *minioGrantProvider) Grant(ctx context.Context, request oci.GrantRequest) (oci.Grant, error) {
	if request.Operation == oci.GrantRead {
		provider.mu.Lock()
		provider.reads = append(provider.reads, request.Key)
		provider.mu.Unlock()
	}
	return oci.Grant{
		Read: func(ctx context.Context) (io.ReadCloser, error) {
			result, err := provider.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &provider.bucket, Key: &request.Key})
			if err != nil {
				return nil, err
			}
			return result.Body, nil
		},
		Write: func(ctx context.Context, reader io.Reader, size int64) error {
			_, err := provider.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket:        &provider.bucket,
				Key:           &request.Key,
				Body:          reader,
				ContentLength: &size,
			})
			return err
		},
	}, nil
}

func (provider *minioGrantProvider) resetReads() {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.reads = nil
}

func (provider *minioGrantProvider) readKeys() []string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]string(nil), provider.reads...)
}
