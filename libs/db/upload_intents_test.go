package db_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStoreReservesUploadIntentIdempotentlyAndResolvesNewestOwnedDigest(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 13, 12, 0, 0, 123456789, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)
	ensureProfileUser(t, ctx, store, "user-2", now)

	first := uploadIntent(t, "upload-1", "user-1", domain.UploadProfileArtifact, digest('a'), 42, now)
	reserved, err := store.ReserveUploadIntent(ctx, first, "upload-key")
	if err != nil {
		t.Fatalf("ReserveUploadIntent(): %v", err)
	}
	replayed, err := store.ReserveUploadIntent(ctx, uploadIntent(t, "unused", "user-1", domain.UploadProfileArtifact, digest('a'), 42, now.Add(time.Minute)), "upload-key")
	if err != nil {
		t.Fatalf("ReserveUploadIntent() replay: %v", err)
	}
	if replayed.Snapshot() != reserved.Snapshot() {
		t.Fatalf("replayed Upload Intent = %#v, want %#v", replayed.Snapshot(), reserved.Snapshot())
	}
	if reserved.Snapshot().CreatedAt.Nanosecond() != 123456000 {
		t.Fatalf("durable creation precision = %d", reserved.Snapshot().CreatedAt.Nanosecond())
	}
	for name, conflict := range map[string]domain.UploadIntent{
		"kind":   uploadIntent(t, "kind", "user-1", domain.UploadGitBundle, digest('a'), 42, now),
		"digest": uploadIntent(t, "digest", "user-1", domain.UploadProfileArtifact, digest('b'), 42, now),
		"size":   uploadIntent(t, "size", "user-1", domain.UploadProfileArtifact, digest('a'), 43, now),
	} {
		t.Run(name+" conflict", func(t *testing.T) {
			if _, err := store.ReserveUploadIntent(ctx, conflict, "upload-key"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
				t.Fatalf("ReserveUploadIntent() error = %v", err)
			}
		})
	}

	newest := uploadIntent(t, "upload-2", "user-1", domain.UploadProfileArtifact, digest('a'), 42, now.Add(2*time.Minute))
	if _, err := store.ReserveUploadIntent(ctx, newest, "new-key"); err != nil {
		t.Fatalf("reserve retry Upload Intent: %v", err)
	}
	loaded, err := store.GetOwnedUploadIntentByDigest(ctx, "user-1", domain.UploadProfileArtifact, digest('a'))
	if err != nil || loaded.Snapshot().ID != "upload-2" {
		t.Fatalf("GetOwnedUploadIntentByDigest() = %#v, %v", loaded.Snapshot(), err)
	}
	for _, lookup := range []struct {
		owner  string
		kind   domain.UploadKind
		digest string
	}{{"user-2", domain.UploadProfileArtifact, digest('a')}, {"user-1", domain.UploadGitBundle, digest('a')}, {"user-1", domain.UploadProfileArtifact, digest('f')}} {
		if _, err := store.GetOwnedUploadIntentByDigest(ctx, lookup.owner, lookup.kind, lookup.digest); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
			t.Fatalf("foreign lookup %#v error = %v", lookup, err)
		}
	}
	var intents, registrations int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM upload_intents), (SELECT count(*) FROM upload_intent_registrations)`).Scan(&intents, &registrations); err != nil {
		t.Fatal(err)
	}
	if intents != 2 || registrations != 2 {
		t.Fatalf("rows = intents:%d registrations:%d", intents, registrations)
	}
}

func TestStoreSerializesConcurrentUploadIntentReservation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)
	start := make(chan struct{})
	results := make(chan domain.UploadIntent, 2)
	errorsOut := make(chan error, 2)
	var group sync.WaitGroup
	candidates := []domain.UploadIntent{
		uploadIntent(t, "upload-a", "user-1", domain.UploadTrackedPatch, digest('c'), 7, now),
		uploadIntent(t, "upload-b", "user-1", domain.UploadTrackedPatch, digest('c'), 7, now),
	}
	for _, candidate := range candidates {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			intent, err := store.ReserveUploadIntent(ctx, candidate, "shared-key")
			results <- intent
			errorsOut <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("concurrent reservation: %v", err)
		}
	}
	var canonical string
	for result := range results {
		if canonical == "" {
			canonical = result.Snapshot().ID
		} else if result.Snapshot().ID != canonical {
			t.Fatalf("concurrent IDs = %q and %q", canonical, result.Snapshot().ID)
		}
	}
	var intents, registrations int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM upload_intents), (SELECT count(*) FROM upload_intent_registrations)`).Scan(&intents, &registrations); err != nil {
		t.Fatal(err)
	}
	if intents != 1 || registrations != 1 {
		t.Fatalf("concurrent rows = intents:%d registrations:%d", intents, registrations)
	}
}

func uploadIntent(t *testing.T, id, owner string, kind domain.UploadKind, contentDigest string, size int64, createdAt time.Time) domain.UploadIntent {
	t.Helper()
	intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: id, OwnerUserID: owner, Kind: kind, Digest: contentDigest, SizeBytes: size,
		ObjectKey: "uploads/" + string(kind) + "/" + id, CreatedAt: createdAt, ExpiresAt: createdAt.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ReserveUploadIntent fixture: %v", err)
	}
	return intent
}
