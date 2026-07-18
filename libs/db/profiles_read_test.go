package db_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStoreGetsOwnedProfileWithAndWithoutHeadVersion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{ID: "user-1", WorkOSUserID: "workos-user-1", DefaultRegion: "us-east-1", ObservedAt: createdAt}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: "profile-1", OwnerUserID: "user-1", Name: "Default", Slug: "default", CreatedAt: createdAt})
	if err != nil {
		t.Fatalf("create domain Profile: %v", err)
	}
	if _, err := store.CreateProfile(ctx, profile, "create-profile-1"); err != nil {
		t.Fatalf("persist Profile: %v", err)
	}

	detail, err := store.GetOwnedProfile(ctx, "user-1", "profile-1")
	if err != nil {
		t.Fatalf("get owned Profile before publication: %v", err)
	}
	if detail.HeadVersionID != nil {
		t.Fatalf("head Version ID = %v, want nil before any publication", detail.HeadVersionID)
	}

	refs := []domain.CapsuleRef{{Ref: "owner/example/capsule@" + digest('a'), FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"component-a"}}}
	if _, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, domain.ProfileVersionPublication{
		ID: "profile-version-1", Digest: domain.ComputeProfileVersionDigest(refs), CapsuleRefs: refs, CreatedAt: createdAt.Add(time.Minute),
	}, "publish-1"); err != nil {
		t.Fatalf("publish Profile Version: %v", err)
	}

	detail, err = store.GetOwnedProfile(ctx, "user-1", "profile-1")
	if err != nil {
		t.Fatalf("get owned Profile after publication: %v", err)
	}
	if detail.HeadVersionID == nil || *detail.HeadVersionID != "profile-version-1" {
		t.Fatalf("head Version ID = %v, want profile-version-1", detail.HeadVersionID)
	}
	if detail.Profile.Snapshot().ID != "profile-1" {
		t.Fatalf("Profile = %#v", detail.Profile.Snapshot())
	}
}

func TestStoreRejectsForeignOrAbsentOwnedProfile(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	for _, ownerID := range []string{"user-1", "user-2"} {
		if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{ID: ownerID, WorkOSUserID: "workos-" + ownerID, DefaultRegion: "us-east-1", ObservedAt: createdAt}); err != nil {
			t.Fatalf("ensure User %q: %v", ownerID, err)
		}
	}
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: "profile-1", OwnerUserID: "user-1", Name: "Default", Slug: "default", CreatedAt: createdAt})
	if err != nil {
		t.Fatalf("create domain Profile: %v", err)
	}
	if _, err := store.CreateProfile(ctx, profile, "create-profile-1"); err != nil {
		t.Fatalf("persist Profile: %v", err)
	}

	if _, err := store.GetOwnedProfile(ctx, "user-2", "profile-1"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign owner Get error = %v, want ErrReferenceNotOwned", err)
	}
	if _, err := store.GetOwnedProfile(ctx, "user-1", "missing-profile"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("absent Profile Get error = %v, want ErrReferenceNotOwned", err)
	}
}

func TestStoreListsOnlyOwnedProfilesOrderedByCreation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	for _, ownerID := range []string{"user-1", "user-2"} {
		if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{ID: ownerID, WorkOSUserID: "workos-" + ownerID, DefaultRegion: "us-east-1", ObservedAt: createdAt}); err != nil {
			t.Fatalf("ensure User %q: %v", ownerID, err)
		}
	}
	for index, spec := range []struct {
		id, ownerID, name, slug string
		createdAt               time.Time
	}{
		{"profile-1", "user-1", "First", "first", createdAt},
		{"profile-2", "user-1", "Second", "second", createdAt.Add(time.Minute)},
		{"profile-foreign", "user-2", "Foreign", "foreign", createdAt},
	} {
		profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: spec.id, OwnerUserID: spec.ownerID, Name: spec.name, Slug: spec.slug, CreatedAt: spec.createdAt})
		if err != nil {
			t.Fatalf("create domain Profile %d: %v", index, err)
		}
		if _, err := store.CreateProfile(ctx, profile, "create-"+spec.id); err != nil {
			t.Fatalf("persist Profile %d: %v", index, err)
		}
	}

	details, nextCursor, err := store.ListOwnedProfiles(ctx, "user-1", nil, 0)
	if err != nil {
		t.Fatalf("list owned Profiles: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("owned Profile count = %d, want 2", len(details))
	}
	if details[0].Profile.Snapshot().ID != "profile-1" || details[1].Profile.Snapshot().ID != "profile-2" {
		t.Fatalf("owned Profile order = %#v", []string{details[0].Profile.Snapshot().ID, details[1].Profile.Snapshot().ID})
	}
	if nextCursor != nil {
		t.Fatalf("next cursor = %#v, want nil once every owned Profile fits on the page", nextCursor)
	}
	empty, emptyCursor, err := store.ListOwnedProfiles(ctx, "user-3", nil, 0)
	if err != nil {
		t.Fatalf("list Profiles for unknown owner: %v", err)
	}
	if len(empty) != 0 || emptyCursor != nil {
		t.Fatalf("unowned Profile list = %#v, cursor = %#v, want empty and no cursor", empty, emptyCursor)
	}
}

// TestStorePaginatesOwnedProfilesWithStableKeysetWalk confirms the keyset
// contract Finding 1 requires: paging through with a small page size visits
// every owned Profile exactly once, in creation order, with no overlap or
// gaps, and replaying the same request is stable.
func TestStorePaginatesOwnedProfilesWithStableKeysetWalk(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{ID: "user-1", WorkOSUserID: "workos-user-1", DefaultRegion: "us-east-1", ObservedAt: createdAt}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	for index := 0; index < 5; index++ {
		id := fmt.Sprintf("profile-%d", index+1)
		profile, err := domain.CreateProfile(domain.ProfileSnapshot{
			ID: id, OwnerUserID: "user-1", Name: id, Slug: id, CreatedAt: createdAt.Add(time.Duration(index) * time.Minute),
		})
		if err != nil {
			t.Fatalf("create domain Profile %d: %v", index, err)
		}
		if _, err := store.CreateProfile(ctx, profile, "create-"+id); err != nil {
			t.Fatalf("persist Profile %d: %v", index, err)
		}
	}

	var cursor *dbstore.Cursor
	var seen []string
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paginated more than 10 times walking 5 Profiles with page size 2; likely stuck in a loop")
		}
		details, next, err := store.ListOwnedProfiles(ctx, "user-1", cursor, 2)
		if err != nil {
			t.Fatalf("list owned Profiles page %d: %v", pages, err)
		}
		for _, detail := range details {
			seen = append(seen, detail.Profile.Snapshot().ID)
		}
		if next == nil {
			break
		}
		cursor = next
	}
	want := []string{"profile-1", "profile-2", "profile-3", "profile-4", "profile-5"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("paginated owned Profile IDs = %#v, want %#v", seen, want)
	}

	// Replaying the very first page must reproduce it exactly: pagination
	// is stable under identical calls.
	replay, replayNext, err := store.ListOwnedProfiles(ctx, "user-1", nil, 2)
	if err != nil {
		t.Fatalf("replay first page: %v", err)
	}
	if len(replay) != 2 || replay[0].Profile.Snapshot().ID != "profile-1" || replay[1].Profile.Snapshot().ID != "profile-2" {
		t.Fatalf("replayed first page = %#v", replay)
	}
	if replayNext == nil {
		t.Fatal("replayed first page next cursor = nil, want non-nil (3 Profiles remain)")
	}
}

// TestStorePaginatesOwnedProfilesDisambiguatingIdenticalCreatedAt confirms
// the keyset boundary case: two Profiles sharing the exact same created_at
// are still split across pages deterministically, ordered and disambiguated
// by id, and neither is skipped nor duplicated at the page boundary.
func TestStorePaginatesOwnedProfilesDisambiguatingIdenticalCreatedAt(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{ID: "user-1", WorkOSUserID: "workos-user-1", DefaultRegion: "us-east-1", ObservedAt: createdAt}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	// All three Profiles share the same created_at; only id can order them.
	for _, id := range []string{"profile-a", "profile-b", "profile-c"} {
		profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: id, OwnerUserID: "user-1", Name: id, Slug: id, CreatedAt: createdAt})
		if err != nil {
			t.Fatalf("create domain Profile %q: %v", id, err)
		}
		if _, err := store.CreateProfile(ctx, profile, "create-"+id); err != nil {
			t.Fatalf("persist Profile %q: %v", id, err)
		}
	}

	first, cursor, err := store.ListOwnedProfiles(ctx, "user-1", nil, 2)
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if len(first) != 2 || first[0].Profile.Snapshot().ID != "profile-a" || first[1].Profile.Snapshot().ID != "profile-b" {
		t.Fatalf("first page = %#v, want [profile-a profile-b] ordered by id under a shared created_at", first)
	}
	if cursor == nil || cursor.ID != "profile-b" {
		t.Fatalf("cursor after first page = %#v, want it to key off profile-b", cursor)
	}

	second, secondCursor, err := store.ListOwnedProfiles(ctx, "user-1", cursor, 2)
	if err != nil {
		t.Fatalf("list second page: %v", err)
	}
	if len(second) != 1 || second[0].Profile.Snapshot().ID != "profile-c" {
		t.Fatalf("second page = %#v, want exactly [profile-c] (no duplicate, no skip at the boundary)", second)
	}
	if secondCursor != nil {
		t.Fatalf("cursor after final page = %#v, want nil", secondCursor)
	}
}

// TestStorePaginatesOwnedProfilesRejectsInvalidCursor confirms a corrupt
// cursor string is reported through DecodeCursor with ErrInvalidCursor
// before the store method reaches the database.
func TestStorePaginatesOwnedProfilesRejectsInvalidCursor(t *testing.T) {
	if _, err := dbstore.DecodeCursor("not-a-valid-cursor"); !errors.Is(err, dbstore.ErrInvalidCursor) {
		t.Fatalf("DecodeCursor() error = %v, want ErrInvalidCursor", err)
	}
	if _, err := dbstore.DecodeCursor(""); !errors.Is(err, dbstore.ErrInvalidCursor) {
		t.Fatalf("DecodeCursor(\"\") error = %v, want ErrInvalidCursor", err)
	}
}

func TestStoreGetsOwnedProfileVersion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	for _, ownerID := range []string{"user-1", "user-2"} {
		if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{ID: ownerID, WorkOSUserID: "workos-" + ownerID, DefaultRegion: "us-east-1", ObservedAt: createdAt}); err != nil {
			t.Fatalf("ensure User %q: %v", ownerID, err)
		}
	}
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: "profile-1", OwnerUserID: "user-1", Name: "Default", Slug: "default", CreatedAt: createdAt})
	if err != nil {
		t.Fatalf("create domain Profile: %v", err)
	}
	if _, err := store.CreateProfile(ctx, profile, "create-profile-1"); err != nil {
		t.Fatalf("persist Profile: %v", err)
	}
	refs := []domain.CapsuleRef{{Ref: "owner/example/capsule@" + digest('a'), FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"component-a"}}}
	if _, err := store.PublishProfileVersion(ctx, "user-1", "profile-1", nil, domain.ProfileVersionPublication{
		ID: "profile-version-1", Digest: domain.ComputeProfileVersionDigest(refs), CapsuleRefs: refs, CreatedAt: createdAt.Add(time.Minute),
	}, "publish-1"); err != nil {
		t.Fatalf("publish Profile Version: %v", err)
	}

	version, err := store.GetOwnedProfileVersion(ctx, "user-1", "profile-version-1")
	if err != nil {
		t.Fatalf("get owned Profile Version: %v", err)
	}
	if version.Snapshot().ID != "profile-version-1" || version.Snapshot().ProfileID != "profile-1" {
		t.Fatalf("Profile Version = %#v", version.Snapshot())
	}
	if _, err := store.GetOwnedProfileVersion(ctx, "user-2", "profile-version-1"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign owner Profile Version Get error = %v, want ErrReferenceNotOwned", err)
	}
	if _, err := store.GetOwnedProfileVersion(ctx, "user-1", "missing-version"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("absent Profile Version Get error = %v, want ErrReferenceNotOwned", err)
	}
}
