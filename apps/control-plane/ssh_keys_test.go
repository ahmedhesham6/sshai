package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

const httpEd25519PublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMzdhbPIA9osmLQz0iTvx/VNJP8fjiD3wfl9LSn2d92"

func TestSSHKeyHTTPTracerRegistersListsAndRevokesPublicIdentity(t *testing.T) {
	now := time.Date(2026, time.July, 13, 15, 0, 0, 0, time.UTC)
	repository := newSSHKeyHTTPRepository()
	handler := sshKeyHandler(repository, []string{"ssh-key-1"}, repeatValue("user-1", 5), []string{"request-register", "request-list", "request-revoke", "request-list-revoked", "request-revoke-replay"}, now)

	created := serveSSHKeyRequest(handler, http.MethodPost, "/v1/ssh-keys", "register-key-0001", `{"label":"Laptop","publicKey":"`+httpEd25519PublicKey+` local-comment"}`)
	if created.Code != http.StatusCreated || created.Header().Get("X-Request-ID") != "request-register" {
		t.Fatalf("register response = status:%d body:%s", created.Code, created.Body.String())
	}
	createdBody := created.Body.Bytes()
	var key contracts.SSHKey
	if err := json.Unmarshal(createdBody, &key); err != nil {
		t.Fatal(err)
	}
	if key.Id != "ssh-key-1" || key.Label != "Laptop" || key.Algorithm != contracts.SshEd25519 || key.PublicKey != httpEd25519PublicKey || key.Fingerprint == "" || !key.CreatedAt.Equal(now) {
		t.Fatalf("created SSH Key = %#v", key)
	}
	assertPublicSSHKeyBody(t, createdBody)

	listed := serveSSHKeyRequest(handler, http.MethodGet, "/v1/ssh-keys", "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list response = status:%d body:%s", listed.Code, listed.Body.String())
	}
	listedBody := listed.Body.Bytes()
	var page contracts.SSHKeyPage
	if err := json.Unmarshal(listedBody, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0] != key || page.NextCursor != nil {
		t.Fatalf("listed SSH Keys = %#v", page)
	}
	assertPublicSSHKeyBody(t, listedBody)

	revoked := serveSSHKeyRequest(handler, http.MethodDelete, "/v1/ssh-keys/ssh-key-1", "revoke-key-00001", "")
	if revoked.Code != http.StatusNoContent || revoked.Body.Len() != 0 {
		t.Fatalf("revoke response = status:%d body:%s", revoked.Code, revoked.Body.String())
	}
	listed = serveSSHKeyRequest(handler, http.MethodGet, "/v1/ssh-keys", "", "")
	if listed.Code != http.StatusOK || listed.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("list revoked response = status:%d body:%s", listed.Code, listed.Body.String())
	}
	replayed := serveSSHKeyRequest(handler, http.MethodDelete, "/v1/ssh-keys/ssh-key-1", "revoke-key-00001", "")
	if replayed.Code != http.StatusNoContent {
		t.Fatalf("revoke replay = status:%d body:%s", replayed.Code, replayed.Body.String())
	}
}

func TestSSHKeyHTTPMapsValidationConflictAndUnavailableSafely(t *testing.T) {
	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       string
		key        string
		repository *sshKeyHTTPRepository
		wantStatus int
		wantCode   string
	}{
		{name: "invalid non Ed25519", method: http.MethodPost, path: "/v1/ssh-keys", key: "register-key-0001", body: `{"label":"Laptop","publicKey":"ssh-rsa AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, repository: newSSHKeyHTTPRepository(), wantStatus: http.StatusBadRequest, wantCode: "INVALID_SSH_KEY"},
		{name: "idempotency conflict", method: http.MethodPost, path: "/v1/ssh-keys", key: "register-key-0001", body: `{"label":"Laptop","publicKey":"` + httpEd25519PublicKey + `"}`, repository: &sshKeyHTTPRepository{err: db.ErrIdempotencyConflict}, wantStatus: http.StatusConflict, wantCode: "IDEMPOTENCY_CONFLICT"},
		{name: "register unavailable", method: http.MethodPost, path: "/v1/ssh-keys", key: "register-key-0001", body: `{"label":"Laptop","publicKey":"` + httpEd25519PublicKey + `"}`, repository: &sshKeyHTTPRepository{err: errors.New("postgres password=secret")}, wantStatus: http.StatusServiceUnavailable, wantCode: "COMMAND_UNAVAILABLE"},
		{name: "list unavailable", method: http.MethodGet, path: "/v1/ssh-keys", repository: &sshKeyHTTPRepository{err: errors.New("postgres password=secret")}, wantStatus: http.StatusServiceUnavailable, wantCode: "COMMAND_UNAVAILABLE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := sshKeyHandler(test.repository, []string{"ssh-key-1"}, []string{"user-1"}, []string{"request-error"}, time.Now())
			response := serveSSHKeyRequest(handler, test.method, test.path, test.key, test.body)
			if response.Code != test.wantStatus || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+test.wantCode+`"`)) || bytes.Contains(response.Body.Bytes(), []byte("password=secret")) {
				t.Fatalf("response = status:%d body:%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSSHKeyHTTPDoesNotDiscloseForeignOwnership(t *testing.T) {
	responses := make([]*httptest.ResponseRecorder, 0, 2)
	for _, repository := range []*sshKeyHTTPRepository{{err: db.ErrReferenceNotOwned}, {err: db.ErrReferenceNotOwned}} {
		handler := sshKeyHandler(repository, []string{"unused"}, []string{"user-1"}, []string{"request-not-found"}, time.Now())
		responses = append(responses, serveSSHKeyRequest(handler, http.MethodDelete, "/v1/ssh-keys/hidden-key", "revoke-key-00001", ""))
	}
	if responses[0].Code != http.StatusNotFound || responses[0].Body.String() != responses[1].Body.String() || !bytes.Contains(responses[0].Body.Bytes(), []byte(`"code":"SSH_KEY_NOT_FOUND"`)) {
		t.Fatalf("non-disclosure responses = first:%d/%s second:%d/%s", responses[0].Code, responses[0].Body.String(), responses[1].Code, responses[1].Body.String())
	}
}

func sshKeyHandler(repository application.SSHKeyRepository, keyIDs, userIDs, requestIDs []string, now time.Time) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		SSHKeys:  application.NewSSHKeyService(repository, &idsFake{values: keyIDs}, func() time.Time { return now }),
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: userIDs},
		RequestIDs: &idsFake{values: requestIDs}, DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
}

func serveSSHKeyRequest(handler http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertPublicSSHKeyBody(t *testing.T, body []byte) {
	t.Helper()
	for _, forbidden := range [][]byte{[]byte("ownerUserId"), []byte("revokedAt"), []byte("privateKey"), []byte("local-comment")} {
		if bytes.Contains(body, forbidden) {
			t.Fatalf("SSH Key response contains private/internal field %q: %s", forbidden, body)
		}
	}
}

func repeatValue(value string, count int) []string {
	values := make([]string, count)
	for index := range values {
		values[index] = value
	}
	return values
}

type sshKeyHTTPRepository struct {
	keys          map[string]domain.SSHKey
	registrations map[string]domain.SSHKey
	revocations   map[string]string
	err           error
}

func newSSHKeyHTTPRepository() *sshKeyHTTPRepository {
	return &sshKeyHTTPRepository{keys: map[string]domain.SSHKey{}, registrations: map[string]domain.SSHKey{}, revocations: map[string]string{}}
}

func (repository *sshKeyHTTPRepository) RegisterSSHKey(_ context.Context, candidate domain.SSHKey, idempotencyKey string) (domain.SSHKey, error) {
	if repository.err != nil {
		return domain.SSHKey{}, repository.err
	}
	if existing, found := repository.registrations[idempotencyKey]; found {
		if existing.Snapshot().Label != candidate.Snapshot().Label || existing.Snapshot().PublicKey != candidate.Snapshot().PublicKey {
			return domain.SSHKey{}, db.ErrIdempotencyConflict
		}
		return existing, nil
	}
	repository.keys[candidate.Snapshot().ID] = candidate
	repository.registrations[idempotencyKey] = candidate
	return candidate, nil
}

func (repository *sshKeyHTTPRepository) ListActiveOwnedSSHKeys(_ context.Context, ownerID string) ([]domain.SSHKey, error) {
	if repository.err != nil {
		return nil, repository.err
	}
	keys := make([]domain.SSHKey, 0, len(repository.keys))
	for _, key := range repository.keys {
		snapshot := key.Snapshot()
		if snapshot.OwnerUserID == ownerID && snapshot.RevokedAt == nil {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (repository *sshKeyHTTPRepository) RevokeOwnedSSHKey(_ context.Context, ownerID, sshKeyID, idempotencyKey string, at time.Time) error {
	if repository.err != nil {
		return repository.err
	}
	if existingID, found := repository.revocations[idempotencyKey]; found {
		if existingID != sshKeyID {
			return db.ErrIdempotencyConflict
		}
		return nil
	}
	key, found := repository.keys[sshKeyID]
	if !found || key.Snapshot().OwnerUserID != ownerID {
		return db.ErrReferenceNotOwned
	}
	revoked, err := key.Revoke(at)
	if err != nil {
		return err
	}
	repository.keys[sshKeyID] = revoked
	repository.revocations[idempotencyKey] = sshKeyID
	return nil
}
