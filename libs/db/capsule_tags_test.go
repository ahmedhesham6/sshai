package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
)

func TestCapsuleTagQueriesAreOwnerScopedIdempotentAndMutable(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	observedAt := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	for _, user := range []dbstore.EnsureUserInput{
		{ID: "user-1", WorkOSUserID: "workos-1", DefaultRegion: "eu-central-1", ObservedAt: observedAt},
		{ID: "user-2", WorkOSUserID: "workos-2", DefaultRegion: "eu-central-1", ObservedAt: observedAt},
	} {
		if _, err := store.EnsureUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	firstDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secondDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	first, err := store.PutCapsuleTag(ctx, "user-1", "agents", "stable", firstDigest, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.PutCapsuleTag(ctx, "user-1", "agents", "stable", firstDigest, observedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("idempotent update time = %s, want %s", replayed.UpdatedAt, first.UpdatedAt)
	}
	retagged, err := store.PutCapsuleTag(ctx, "user-1", "agents", "stable", secondDigest, observedAt.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if retagged.Digest != secondDigest || !retagged.UpdatedAt.Equal(observedAt.Add(2*time.Minute)) {
		t.Fatalf("retagged record = %#v", retagged)
	}
	resolved, err := store.GetCapsuleTag(ctx, "user-1", "agents", "stable")
	if err != nil || resolved.Digest != secondDigest {
		t.Fatalf("resolved record = %#v, error = %v", resolved, err)
	}
	if _, err := store.GetCapsuleTag(ctx, "user-2", "agents", "stable"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign owner error = %v, want ErrReferenceNotOwned", err)
	}
}
