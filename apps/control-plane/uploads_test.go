package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestCreateUploadIntentHTTPReturnsCompleteSignedRequest(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &uploadHTTPRepositoryFake{}
	signer := &uploadHTTPSignerFake{signed: application.SignedUpload{
		URL: "https://objects.example/uploads/object?signature=ok",
		RequiredHeaders: map[string]string{
			"Content-Length":          "12",
			"X-Amz-Checksum-Sha256":   "checksum",
			"X-Amz-Meta-Sshai-Digest": testHTTPDigest('a'),
			"X-Amz-Meta-Sshai-Kind":   string(domain.UploadProfileArtifact),
		},
	}}
	handler := uploadHandler(repository, signer, now)
	request := uploadRequest(`{"kind":"profile_artifact","digest":"` + testHTTPDigest('a') + `","sizeBytes":12}`)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || response.Header().Get("X-Request-ID") != "request-upload" {
		t.Fatalf("response = status:%d request:%q body:%s", response.Code, response.Header().Get("X-Request-ID"), response.Body.String())
	}
	var created struct {
		UploadID        string            `json:"uploadId"`
		URL             string            `json:"url"`
		ExpiresAt       time.Time         `json:"expiresAt"`
		RequiredHeaders map[string]string `json:"requiredHeaders"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.UploadID != "upload-1" || created.URL != signer.signed.URL || !created.ExpiresAt.Equal(now.Add(10*time.Minute)) || len(created.RequiredHeaders) != 4 || created.RequiredHeaders["Content-Length"] != "12" {
		t.Fatalf("created Upload Intent = %#v", created)
	}
	snapshot := repository.candidate.Snapshot()
	if snapshot.OwnerUserID != "user-1" || snapshot.Kind != domain.UploadProfileArtifact || snapshot.SizeBytes != 12 || repository.key != "upload-intent-key-1" || signer.intent.Snapshot().ID != "upload-1" {
		t.Fatalf("command = intent:%#v key:%q signed:%#v", snapshot, repository.key, signer.intent.Snapshot())
	}
}

func TestCreateUploadIntentHTTPMapsSafeErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		repository error
		signer     error
		size       int64
		wantStatus int
		wantCode   string
	}{
		{name: "invalid intent", size: 101, wantStatus: http.StatusBadRequest, wantCode: "INVALID_UPLOAD"},
		{name: "idempotency conflict", repository: db.ErrIdempotencyConflict, size: 12, wantStatus: http.StatusConflict, wantCode: "IDEMPOTENCY_CONFLICT"},
		{name: "provider unavailable", signer: errors.New("endpoint password=secret"), size: 12, wantStatus: http.StatusServiceUnavailable, wantCode: "COMMAND_UNAVAILABLE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &uploadHTTPRepositoryFake{err: test.repository}
			signer := &uploadHTTPSignerFake{err: test.signer, signed: application.SignedUpload{URL: "https://objects.example/upload"}}
			handler := uploadHandler(repository, signer, time.Now())
			request := uploadRequest(`{"kind":"profile_artifact","digest":"` + testHTTPDigest('a') + `","sizeBytes":` + strconv.FormatInt(test.size, 10) + `}`)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+test.wantCode+`"`)) {
				t.Fatalf("response = status:%d body:%s", response.Code, response.Body.String())
			}
			if bytes.Contains(response.Body.Bytes(), []byte("password=secret")) {
				t.Fatalf("unsafe response = %s", response.Body.String())
			}
		})
	}
}

func uploadHandler(repository application.UploadIntentRepository, signer application.UploadSigner, now time.Time) http.Handler {
	service := application.NewUploadIntentService(
		repository, signer, nil, nil, &idsFake{values: []string{"upload-1"}}, func() time.Time { return now }, 10*time.Minute,
		map[domain.UploadKind]int64{domain.UploadProfileArtifact: 100},
	)
	return controlplane.NewHandler(controlplane.Config{
		Uploads: service, Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
		RequestIDs: &idsFake{values: []string{"request-upload"}}, DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
}

func uploadRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/uploads", bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "upload-intent-key-1")
	return request
}

type uploadHTTPRepositoryFake struct {
	candidate domain.UploadIntent
	key       string
	err       error
}

func (repository *uploadHTTPRepositoryFake) ReserveUploadIntent(_ context.Context, candidate domain.UploadIntent, key string) (domain.UploadIntent, error) {
	repository.candidate, repository.key = candidate, key
	return candidate, repository.err
}

func (*uploadHTTPRepositoryFake) GetOwnedUploadIntentByDigest(context.Context, string, domain.UploadKind, string) (domain.UploadIntent, error) {
	return domain.UploadIntent{}, errors.New("not used")
}

type uploadHTTPSignerFake struct {
	intent domain.UploadIntent
	signed application.SignedUpload
	err    error
}

type successfulUploadVerifier struct {
	size int64
	err  error
}

func (verifier *successfulUploadVerifier) Verify(_ context.Context, input application.VerifyUploadInput) (application.VerifiedUpload, error) {
	if verifier.err != nil {
		return application.VerifiedUpload{}, verifier.err
	}
	now := time.Now()
	intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-verified", OwnerUserID: input.OwnerUserID, Kind: input.Kind, Digest: input.Digest, SizeBytes: verifier.size,
		ObjectKey: "uploads/" + string(input.Kind) + "/verified", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		return application.VerifiedUpload{}, err
	}
	return application.VerifiedUpload{Intent: intent, ObjectKey: "objects/final"}, nil
}

func (signer *uploadHTTPSignerFake) SignUpload(_ context.Context, intent domain.UploadIntent) (application.SignedUpload, error) {
	signer.intent = intent
	return signer.signed, signer.err
}

func testHTTPDigest(character byte) string {
	return "sha256:" + string(bytes.Repeat([]byte{character}, 64))
}
