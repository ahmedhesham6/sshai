package db_test

import (
	"context"
	"errors"
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

	details, err := store.ListOwnedProfiles(ctx, "user-1")
	if err != nil {
		t.Fatalf("list owned Profiles: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("owned Profile count = %d, want 2", len(details))
	}
	if details[0].Profile.Snapshot().ID != "profile-1" || details[1].Profile.Snapshot().ID != "profile-2" {
		t.Fatalf("owned Profile order = %#v", []string{details[0].Profile.Snapshot().ID, details[1].Profile.Snapshot().ID})
	}
	empty, err := store.ListOwnedProfiles(ctx, "user-3")
	if err != nil {
		t.Fatalf("list Profiles for unknown owner: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unowned Profile list = %#v, want empty", empty)
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
