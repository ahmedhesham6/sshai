package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStoreCreatesProfileIdempotently(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)

	created, err := store.CreateProfile(ctx, newProfile(t, "profile-1", "user-1", "Personal", "personal", now), "create-key")
	if err != nil {
		t.Fatalf("CreateProfile(): %v", err)
	}
	replayed, err := store.CreateProfile(ctx, newProfile(t, "unused", "user-1", "Personal", "personal", now.Add(time.Minute)), "create-key")
	if err != nil {
		t.Fatalf("CreateProfile() replay: %v", err)
	}
	if replayed.Snapshot().ID != created.Snapshot().ID || !replayed.Snapshot().CreatedAt.Equal(now) {
		t.Fatalf("replayed Profile = %#v, want original %#v", replayed.Snapshot(), created.Snapshot())
	}
	if _, err := store.CreateProfile(ctx, newProfile(t, "conflict", "user-1", "Work", "work", now), "create-key"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	if _, err := store.CreateProfile(ctx, newProfile(t, "duplicate", "user-1", "Personal", "personal", now), "another-key"); !errors.Is(err, dbstore.ErrProfileConflict) {
		t.Fatalf("duplicate slug error = %v", err)
	}
}

func TestStorePublishesProfileVersionIdempotentlyAndOwnerScoped(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)
	ensureProfileUser(t, ctx, store, "user-2", now)
	if _, err := store.CreateProfile(ctx, newProfile(t, "profile-1", "user-1", "Personal", "personal", now), "create-key"); err != nil {
		t.Fatalf("create Profile: %v", err)
	}

	first, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, publication("version-1", "artifact-1", 'a', now), "publish-key")
	if err != nil {
		t.Fatalf("PublishProfileVersion(): %v", err)
	}
	replayed, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, publication("unused-version", "unused-artifact", 'a', now.Add(time.Minute)), "publish-key")
	if err != nil {
		t.Fatalf("PublishProfileVersion() replay: %v", err)
	}
	if replayed.Snapshot().ID != first.Snapshot().ID || replayed.Snapshot().Artifacts[0].ID != "artifact-1" || replayed.Snapshot().Artifacts[0].SizeBytes != 42 || replayed.Snapshot().Artifacts[0].Mode != 0o640 {
		t.Fatalf("replayed Profile Version = %#v, want original %#v", replayed.Snapshot(), first.Snapshot())
	}
	if _, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, publication("conflict-version", "conflict-artifact", 'f', now), "publish-key"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	sizeConflict := publication("unused-size-version", "unused-size-artifact", 'a', now)
	sizeConflict.Artifacts[0].SizeBytes++
	if _, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, sizeConflict, "publish-key"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("size-only replay conflict error = %v", err)
	}
	modeConflict := publication("unused-mode-version", "unused-mode-artifact", 'a', now)
	modeConflict.Artifacts[0].Mode = 0o600
	if _, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, modeConflict, "publish-key"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("mode-only replay conflict error = %v", err)
	}
	if _, err := store.PublishProfileVersion(ctx, "user-2", "profile-1", nil, publication("hidden-version", "hidden-artifact", 'b', now), "hidden-key"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("cross-owner publication error = %v", err)
	}
}

func TestStoreSerializesProfileHeadPublication(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)
	if _, err := store.CreateProfile(ctx, newProfile(t, "profile-1", "user-1", "Personal", "personal", now), "create-key"); err != nil {
		t.Fatalf("create Profile: %v", err)
	}
	first, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, publication("version-1", "artifact-1", 'a', now), "first-key")
	if err != nil {
		t.Fatalf("publish first Profile Version: %v", err)
	}
	headID := first.Snapshot().ID

	start := make(chan struct{})
	results := make(chan error, 2)
	for index, character := range []byte{'b', 'c'} {
		go func() {
			<-start
			_, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", &headID,
				publication(fmt.Sprintf("version-%d", index+2), fmt.Sprintf("artifact-%d", index+2), character, now.Add(time.Minute)),
				fmt.Sprintf("concurrent-key-%d", index))
			results <- err
		}()
	}
	close(start)
	firstErr, secondErr := <-results, <-results
	if (firstErr == nil) == (secondErr == nil) || (!errors.Is(firstErr, domain.ErrStaleProfileHead) && !errors.Is(secondErr, domain.ErrStaleProfileHead)) {
		t.Fatalf("concurrent publication errors = (%v, %v), want one success and one stale head", firstErr, secondErr)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM profile_versions WHERE profile_id = 'profile-1'`).Scan(&count); err != nil {
		t.Fatalf("count Profile Versions: %v", err)
	}
	if count != 2 {
		t.Fatalf("Profile Version count = %d, want 2", count)
	}
}

func ensureProfileUser(t *testing.T, ctx context.Context, store *dbstore.Store, id string, now time.Time) {
	t.Helper()
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{ID: id, WorkOSUserID: "workos-" + id, DefaultRegion: "us-east-1", ObservedAt: now}); err != nil {
		t.Fatalf("ensure User %q: %v", id, err)
	}
}

func newProfile(t *testing.T, id, ownerID, name, slug string, createdAt time.Time) domain.Profile {
	t.Helper()
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: id, OwnerUserID: ownerID, Name: name, Slug: slug, CreatedAt: createdAt})
	if err != nil {
		t.Fatalf("create Profile fixture: %v", err)
	}
	return profile
}

func publication(versionID, artifactID string, character byte, createdAt time.Time) domain.ProfileVersionPublication {
	return domain.ProfileVersionPublication{
		ID: versionID, Digest: digest(character), CreatedAt: createdAt,
		Artifacts: []domain.ProfileArtifact{{
			ID: artifactID, ProfileVersionID: versionID, Kind: domain.ArtifactAgentInstruction,
			SourceLocator: "AGENTS.md#$", SourceDigest: digest(character), ContentDigest: digest(character),
			SizeBytes: 42, Mode: 0o640,
			Sensitivity: domain.SensitivityPrivate, Trust: domain.TrustUserAuthored,
		}},
	}
}
