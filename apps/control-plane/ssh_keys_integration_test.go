//go:build !race

package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSSHKeyHTTPAndPostgresTracerPreservesPublicOwnershipAndReplay(t *testing.T) {
	ctx := context.Background()
	database, connectionString := testfixtures.OpenPostgres(t, ctx)
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	pool, err := pgxpool.New(ctx, connectionString)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	store := db.NewStore(pool)
	now := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	service := application.NewSSHKeyService(store, &idsFake{values: []string{"ssh-key-1", "unused-key-1", "unused-key-2"}}, func() time.Time { return now })
	ownerHandler := postgresSSHKeyHandler(service, store, "workos-user-1", repeatValue("user-1", 7), []string{
		"request-register", "request-register-replay", "request-register-conflict", "request-list",
		"request-revoke", "request-revoke-replay", "request-list-revoked",
	}, now)

	var registered contracts.SSHKey
	for attempt := 0; attempt < 2; attempt++ {
		response := serveSSHKeyRequest(ownerHandler, http.MethodPost, "/v1/ssh-keys", "register-key-0001", `{"label":"Laptop","publicKey":"`+httpEd25519PublicKey+` local-comment"}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("register attempt %d = status:%d body:%s", attempt+1, response.Code, response.Body.String())
		}
		responseBody := response.Body.Bytes()
		var key contracts.SSHKey
		if err := json.Unmarshal(responseBody, &key); err != nil {
			t.Fatalf("decode register attempt %d: %v", attempt+1, err)
		}
		if attempt == 0 {
			registered = key
		} else if key != registered {
			t.Fatalf("register replay = %#v, want %#v", key, registered)
		}
		assertPublicSSHKeyBody(t, responseBody)
	}
	conflict := serveSSHKeyRequest(ownerHandler, http.MethodPost, "/v1/ssh-keys", "register-key-0001", `{"label":"Different","publicKey":"`+httpEd25519PublicKey+`"}`)
	if conflict.Code != http.StatusConflict || !bytes.Contains(conflict.Body.Bytes(), []byte(`"code":"IDEMPOTENCY_CONFLICT"`)) {
		t.Fatalf("registration conflict = status:%d body:%s", conflict.Code, conflict.Body.String())
	}
	listed := serveSSHKeyRequest(ownerHandler, http.MethodGet, "/v1/ssh-keys", "", "")
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte(`"id":"ssh-key-1"`)) {
		t.Fatalf("owner list = status:%d body:%s", listed.Code, listed.Body.String())
	}
	assertPublicSSHKeyBody(t, listed.Body.Bytes())

	foreignHandler := postgresSSHKeyHandler(service, store, "workos-user-2", repeatValue("user-2", 3), repeatValue("request-hidden", 3), now)
	foreignList := serveSSHKeyRequest(foreignHandler, http.MethodGet, "/v1/ssh-keys", "", "")
	if foreignList.Code != http.StatusOK || foreignList.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("foreign list = status:%d body:%s", foreignList.Code, foreignList.Body.String())
	}
	foreign := serveSSHKeyRequest(foreignHandler, http.MethodDelete, "/v1/ssh-keys/ssh-key-1", "revoke-key-00001", "")
	missing := serveSSHKeyRequest(foreignHandler, http.MethodDelete, "/v1/ssh-keys/missing-key", "missing-key-00001", "")
	if foreign.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() || !bytes.Contains(foreign.Body.Bytes(), []byte(`"code":"SSH_KEY_NOT_FOUND"`)) {
		t.Fatalf("ownership disclosure = foreign:%d/%s missing:%d/%s", foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String())
	}

	for attempt := 0; attempt < 2; attempt++ {
		response := serveSSHKeyRequest(ownerHandler, http.MethodDelete, "/v1/ssh-keys/ssh-key-1", "revoke-key-00001", "")
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Fatalf("revoke attempt %d = status:%d body:%s", attempt+1, response.Code, response.Body.String())
		}
	}
	listed = serveSSHKeyRequest(ownerHandler, http.MethodGet, "/v1/ssh-keys", "", "")
	if listed.Code != http.StatusOK || listed.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("revoked list = status:%d body:%s", listed.Code, listed.Body.String())
	}
	var keys, registrations int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ssh_keys), (SELECT count(*) FROM ssh_key_registrations)`).Scan(&keys, &registrations); err != nil {
		t.Fatalf("count SSH Key rows: %v", err)
	}
	if keys != 1 || registrations != 2 {
		t.Fatalf("persisted rows = SSH Keys:%d registrations:%d", keys, registrations)
	}
}

func postgresSSHKeyHandler(service *application.SSHKeyService, users controlplane.UserProjection, workOSUserID string, userIDs, requestIDs []string, now time.Time) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		SSHKeys: service, Verifier: sshKeySubjectVerifier{workOSUserID: workOSUserID}, Users: users,
		UserIDs: &idsFake{values: userIDs}, RequestIDs: &idsFake{values: requestIDs},
		DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
}

type sshKeySubjectVerifier struct{ workOSUserID string }

func (verifier sshKeySubjectVerifier) Verify(context.Context, string) (auth.Subject, error) {
	return auth.Subject{WorkOSUserID: verifier.workOSUserID}, nil
}
