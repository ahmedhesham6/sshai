//go:build !race

package controlplane_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProfileHTTPAndPostgresTracerReplaysImmutablePublication(t *testing.T) {
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
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	service := application.NewProfileService(store, &successfulUploadVerifier{size: 42}, &idsFake{values: []string{
		"profile-1", "unused-profile", "version-1", "artifact-1", "unused-version", "unused-artifact",
	}}, func() time.Time { return now })
	handler := controlplane.NewHandler(controlplane.Config{
		Profiles: service, Verifier: verifierFake{}, Users: store,
		UserIDs:       &idsFake{values: []string{"user-1", "unused-user", "unused-user", "unused-user"}},
		RequestIDs:    &idsFake{values: []string{"request-create", "request-create-replay", "request-publish", "request-publish-replay"}},
		DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})

	for attempt := 0; attempt < 2; attempt++ {
		response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles", "profile-create-key", `{"name":"Personal"}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("create attempt %d = status:%d body:%s", attempt+1, response.Code, response.Body.String())
		}
		var profile contracts.ProfileSummary
		if err := json.NewDecoder(response.Body).Decode(&profile); err != nil {
			t.Fatalf("decode create attempt %d: %v", attempt+1, err)
		}
		if profile.Id != "profile-1" {
			t.Fatalf("create attempt %d Profile ID = %q, want stable profile-1", attempt+1, profile.Id)
		}
	}
	publicationBody := validProfilePublicationBody()
	for attempt := 0; attempt < 2; attempt++ {
		response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-publish-key", publicationBody)
		if response.Code != http.StatusCreated {
			t.Fatalf("publish attempt %d = status:%d body:%s", attempt+1, response.Code, response.Body.String())
		}
		var version contracts.ProfileVersion
		if err := json.NewDecoder(response.Body).Decode(&version); err != nil {
			t.Fatalf("decode publish attempt %d: %v", attempt+1, err)
		}
		if version.Id != "version-1" || len(version.Artifacts) != 1 || version.Artifacts[0].Id != "artifact-1" {
			t.Fatalf("publish attempt %d = %#v, want stable server-owned IDs", attempt+1, version)
		}
	}
	var profiles, versions, artifacts, registrations int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM profiles), (SELECT count(*) FROM profile_versions),
		       (SELECT count(*) FROM profile_artifacts), (SELECT count(*) FROM profile_publication_registrations)
	`).Scan(&profiles, &versions, &artifacts, &registrations); err != nil {
		t.Fatalf("count persisted Profile publication: %v", err)
	}
	if profiles != 1 || versions != 1 || artifacts != 1 || registrations != 1 {
		t.Fatalf("persisted rows = Profiles:%d Versions:%d Artifacts:%d registrations:%d", profiles, versions, artifacts, registrations)
	}
}
