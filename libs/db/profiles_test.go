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

	first, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, publication("version-1", "registry.example.com/team/base:stable", 'a', now), "publish-key")
	if err != nil {
		t.Fatalf("PublishProfileVersion(): %v", err)
	}
	replayed, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, publication("unused-version", "registry.example.com/team/base:stable", 'a', now.Add(time.Minute)), "publish-key")
	if err != nil {
		t.Fatalf("PublishProfileVersion() replay: %v", err)
	}
	replayedSnapshot := replayed.Snapshot()
	if replayedSnapshot.ID != first.Snapshot().ID || len(replayedSnapshot.CapsuleRefs) != 1 || replayedSnapshot.CapsuleRefs[0].Ref != "registry.example.com/team/base:stable" || replayedSnapshot.CapsuleRefs[0].FreshnessPolicy != domain.FreshnessReview || len(replayedSnapshot.CapsuleRefs[0].Exclusions) != 2 || replayedSnapshot.CapsuleRefs[0].Exclusions[0] != "config:editor" || replayedSnapshot.CapsuleRefs[0].Exclusions[1] != "skill:debug" {
		t.Fatalf("replayed Profile Version = %#v, want original %#v", replayed.Snapshot(), first.Snapshot())
	}
	if _, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, publication("conflict-version", "registry.example.com/team/other:stable", 'f', now), "publish-key"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	refConflict := publication("unused-ref-version", "registry.example.com/team/other:stable", 'a', now)
	if _, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, refConflict, "publish-key"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("ref-only replay conflict error = %v", err)
	}
	if _, err := store.PublishProfileVersion(ctx, "user-2", "profile-1", nil, publication("hidden-version", "registry.example.com/team/base:stable", 'b', now), "hidden-key"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
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
	first, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, publication("version-1", "registry.example.com/team/base:stable", 'a', now), "first-key")
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
				publication(fmt.Sprintf("version-%d", index+2), "registry.example.com/team/tools:stable", character, now.Add(time.Minute)),
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

func TestStoreRestoresCapsuleRefsFromImmutableVersionRows(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)
	if _, err := store.CreateProfile(ctx, newProfile(t, "profile-1", "user-1", "Personal", "personal", now), "create-key"); err != nil {
		t.Fatalf("create Profile: %v", err)
	}
	original := publication("version-1", "registry.example.com/team/base:stable", 'a', now)
	original.CapsuleRefs[0].FreshnessPolicy = domain.FreshnessReview
	original.CapsuleRefs[0].Exclusions = []string{"config:editor", "skill:debug"}
	if _, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, original, "publish-key"); err != nil {
		t.Fatalf("publish Profile Version: %v", err)
	}

	mutatedRef := "registry.example.com/team/attacker:stable"
	if _, err := pool.Exec(ctx, `
		UPDATE profile_publication_registrations
		SET input = jsonb_set(input, '{capsuleRefs,0,ref}', to_jsonb($1::text))
		WHERE owner_user_id = 'user-1' AND idempotency_key = 'publish-key'`, mutatedRef); err != nil {
		t.Fatalf("mutate idempotency registration fixture: %v", err)
	}
	mutatedInput := original
	mutatedInput.ID = "unused-version"
	mutatedInput.CapsuleRefs = append([]domain.CapsuleRef(nil), original.CapsuleRefs...)
	mutatedInput.CapsuleRefs[0].Ref = mutatedRef
	replayed, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, mutatedInput, "publish-key")
	if err != nil {
		t.Fatalf("replay Profile Version: %v", err)
	}
	got := replayed.Snapshot().CapsuleRefs
	if len(got) != 1 || got[0].Ref != original.CapsuleRefs[0].Ref || got[0].FreshnessPolicy != domain.FreshnessReview || len(got[0].Exclusions) != 2 || got[0].Exclusions[0] != "config:editor" || got[0].Exclusions[1] != "skill:debug" {
		t.Fatalf("replayed Capsule Refs = %#v, want immutable publication refs %#v", got, original.CapsuleRefs)
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

func publication(versionID, ref string, character byte, createdAt time.Time) domain.ProfileVersionPublication {
	return domain.ProfileVersionPublication{
		ID: versionID, Digest: digest(character), CreatedAt: createdAt,
		CapsuleRefs: []domain.CapsuleRef{{Ref: ref, FreshnessPolicy: domain.FreshnessReview, Exclusions: []string{"config:editor", "skill:debug"}}},
	}
}
