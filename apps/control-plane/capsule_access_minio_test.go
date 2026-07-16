package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	orasoci "oras.land/oras-go/v2/content/oci"
)

const capsuleAccessMinioImage = "minio/minio:RELEASE.2024-06-13T22-53-53Z"

func TestCapsuleAccessGrantProviderSupportsOCIPublishAndPullAgainstMinIO(t *testing.T) {
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Skipf("Docker socket is unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	container, err := testcontainers.Run(
		ctx,
		capsuleAccessMinioImage,
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
	bucket := "capsule-access-test"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatalf("create MinIO bucket: %v", err)
	}
	presigner := s3.NewPresignClient(s3Client)
	handler := controlplane.NewHandler(controlplane.Config{
		CapsulePresigner: presigner,
		CapsuleOwnership: controlplane.NewS3CapsuleOwnership(s3Client, bucket),
		CapsuleBucket:    bucket,
		CapsuleAccessTTL: 15 * time.Minute,
		Verifier:         verifierFake{},
		Users:            &usersFake{},
		UserIDs:          &repeatingIDs{value: "user-1"},
		RequestIDs:       &repeatingIDs{value: "request-minio"},
		DefaultRegion:    "us-east-1",
		Now:              time.Now,
	})
	provider := &httpCapsuleGrantProvider{handler: handler, client: http.DefaultClient}
	client, err := oci.NewClient("user-1", provider, oci.WithParallelism(3))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	value := buildMinIOCapsule(t)
	publication, err := client.Publish(ctx, value)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if publication.IndexKey != oci.IndexKey("user-1", value.Digest) || len(publication.BlobKeys) < 2 {
		t.Fatalf("publication = %#v, want owner-scoped index and blobs", publication)
	}
	if _, err := client.Publish(ctx, value); err == nil {
		t.Fatal("second Publish() error = nil, want conditional-write rejection")
	}
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create OCI destination: %v", err)
	}
	pulled, err := client.Pull(ctx, value.Digest, destination, nil)
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if !reflect.DeepEqual(pulled, value) {
		t.Fatalf("pulled Capsule = %#v, want %#v", pulled, value)
	}
}

func buildMinIOCapsule(t *testing.T) capsule.Capsule {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte("{\"editor\":\"vim\"}\n"), 0o644); err != nil {
		t.Fatalf("write Capsule component: %v", err)
	}
	value, err := capsule.NewBuilder(0).Build(capsule.Manifest{
		SchemaVersion: 1,
		Name:          "minio-round-trip",
		Requirements:  capsule.Requirements{Commands: []string{}, Secrets: []string{}},
		Components: []capsule.Component{{
			ID: "config:editor", Type: capsule.ComponentTypeConfig,
			Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative,
			Requirements: capsule.Requirements{Commands: []string{}, Secrets: []string{}},
		}},
	}, map[string]string{"config:editor": root})
	if err != nil {
		t.Fatalf("build Capsule: %v", err)
	}
	return value
}

type httpCapsuleGrantProvider struct {
	handler http.Handler
	client  *http.Client
}

func (provider *httpCapsuleGrantProvider) Grant(ctx context.Context, request oci.GrantRequest) (oci.Grant, error) {
	if request.OwnerID != "user-1" || !strings.HasPrefix(request.Key, "owner/user-1/") {
		return oci.Grant{}, fmt.Errorf("grant request escaped owner namespace: %#v", request)
	}
	digest, err := digestFromOCIKey(request.Key)
	if err != nil {
		return oci.Grant{}, err
	}
	kind := contracts.Blob
	if strings.Contains(request.Key, "/index/manifest/") {
		kind = contracts.Index
	}
	intent := contracts.Pull
	if request.Operation == oci.GrantWrite {
		intent = contracts.Push
	}
	body, err := json.Marshal(contracts.CapsuleAccessRequest{
		Intent:  intent,
		Objects: []contracts.CapsuleAccessObject{{Kind: kind, Digest: digest}},
	})
	if err != nil {
		return oci.Grant{}, err
	}
	apiRequest := httptest.NewRequest(http.MethodPost, "/v1/capsule-access", bytes.NewReader(body))
	apiRequest.Header.Set("Authorization", "Bearer valid-token")
	apiRequest.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	provider.handler.ServeHTTP(response, apiRequest)
	if response.Code != http.StatusOK {
		return oci.Grant{}, fmt.Errorf("capsule-access status %d: %s", response.Code, response.Body.String())
	}
	var result contracts.CapsuleAccessResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return oci.Grant{}, fmt.Errorf("decode capsule-access response: %w", err)
	}
	if len(result.Grants) != 1 {
		return oci.Grant{}, fmt.Errorf("capsule-access grants = %d, want one", len(result.Grants))
	}
	grant := result.Grants[0]
	return oci.Grant{
		ExpiresAt: grant.ExpiresAt,
		Read: func(ctx context.Context) (io.ReadCloser, error) {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, grant.Url, nil)
			if err != nil {
				return nil, err
			}
			for name, value := range grant.Headers {
				request.Header.Set(name, value)
			}
			response, err := provider.client.Do(request)
			if err != nil {
				return nil, err
			}
			if response.StatusCode != http.StatusOK {
				defer response.Body.Close()
				message, _ := io.ReadAll(response.Body)
				return nil, fmt.Errorf("GET %s returned %d: %s", request.URL.Path, response.StatusCode, message)
			}
			return response.Body, nil
		},
		Write: func(ctx context.Context, reader io.Reader, size int64) error {
			data, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			if int64(len(data)) != size {
				return fmt.Errorf("write size = %d, want %d", len(data), size)
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPut, grant.Url, bytes.NewReader(data))
			if err != nil {
				return err
			}
			request.ContentLength = size
			for name, value := range grant.Headers {
				request.Header.Set(name, value)
			}
			response, err := provider.client.Do(request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
				message, _ := io.ReadAll(response.Body)
				return fmt.Errorf("PUT %s returned %d: %s", request.URL.Path, response.StatusCode, message)
			}
			return nil
		},
	}, nil
}

func digestFromOCIKey(key string) (string, error) {
	parts := strings.Split(strings.Trim(key, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "sha256" || len(parts[len(parts)-1]) != 64 {
		return "", fmt.Errorf("invalid OCI key %q", key)
	}
	return "sha256:" + parts[len(parts)-1], nil
}

type repeatingIDs struct {
	value string
}

func (ids *repeatingIDs) NewID() string {
	return ids.value
}
