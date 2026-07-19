package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreConnectionIntentIdempotency(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	insertReadyConnectionIntentState(t, ctx, pool, now)
	prepareCalls, mintCalls := 0, 0
	prepare := func(context.Context) (*string, error) { prepareCalls++; return nil, nil }
	mint := func() string { mintCalls++; return "intent-1" }

	first, err := store.CreateOrReplayConnectionIntent(ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute), prepare, mint)
	if err != nil {
		t.Fatalf("create Connection Intent: %v", err)
	}
	replayed, err := store.CreateOrReplayConnectionIntent(ctx, "user-1", "connection-key-0001", "environment-1", now.Add(30*time.Second), now.Add(90*time.Second), prepare, func() string {
		mintCalls++
		return "intent-unused"
	})
	if err != nil || replayed.IntentID != first.IntentID || !replayed.ExpiresAt.Equal(first.ExpiresAt) || prepareCalls != 1 || mintCalls != 1 {
		t.Fatalf("replay = %#v error:%v prepare:%d mint:%d", replayed, err, prepareCalls, mintCalls)
	}
	if _, err := store.CreateOrReplayConnectionIntent(ctx, "user-1", "connection-key-0001", "environment-2", now.Add(30*time.Second), now.Add(90*time.Second), prepare, mint); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("cross-Environment replay error = %v", err)
	}
}

func TestStoreConnectionIntentExpiryReplacesAndPrunes(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	insertReadyConnectionIntentState(t, ctx, pool, now)
	created, err := store.CreateOrReplayConnectionIntent(ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute), func(context.Context) (*string, error) { return nil, nil }, func() string { return "intent-1" })
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := store.CreateOrReplayConnectionIntent(ctx, "user-1", "connection-key-0001", "environment-1", created.ExpiresAt, created.ExpiresAt.Add(time.Minute), func(context.Context) (*string, error) { return nil, nil }, func() string { return "intent-2" })
	if err != nil || replaced.IntentID != "intent-2" || !replaced.ExpiresAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("replacement = %#v error:%v", replaced, err)
	}
	pruned, err := store.PruneConnectionIntents(ctx, replaced.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM connection_intent_idempotency`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if pruned != 1 || remaining != 0 {
		t.Fatalf("pruned/remaining = %d/%d, want 1/0", pruned, remaining)
	}
}

func TestStoreConnectionIntentPersistsAndReplaysStartOperation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Round(0).UTC()
	insertReadyConnectionIntentState(t, ctx, pool, now)
	start := runtimeOperationCandidate(t, "operation-start", "environment-1", domain.OperationRuntimeStart, "system:connection-start:test", []byte(`{}`), now.Add(time.Second))
	if _, err := store.ReserveRuntimeOperation(ctx, start); err != nil {
		t.Fatal(err)
	}
	operationID := "operation-start"
	created, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(context.Context) (*string, error) { return &operationID, nil }, func() string { return "intent-1" },
	)
	if err != nil || created.OperationID == nil || *created.OperationID != operationID {
		t.Fatalf("created Connection Intent = %#v error:%v", created, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE operations
		SET status = 'succeeded', restate_invocation_id = 'invocation-start', completed_at = $2
		WHERE id = $1`, operationID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now.Add(30*time.Second), now.Add(90*time.Second),
		func(context.Context) (*string, error) { t.Fatal("replay prepared a second start"); return nil, nil },
		func() string { t.Fatal("replay minted a second intent"); return "" },
	)
	if err != nil || replayed.IntentID != created.IntentID || replayed.OperationID == nil || *replayed.OperationID != operationID {
		t.Fatalf("replayed Connection Intent = %#v error:%v", replayed, err)
	}
}

func TestStoreConnectionIntentAndOperationShareIdempotencyScope(t *testing.T) {
	tests := []struct {
		name        string
		intentFirst bool
	}{
		{name: "Connection Intent blocks Operation", intentFirst: true},
		{name: "Operation blocks Connection Intent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, pool := openTestStoreAndPool(t, ctx)
			now := time.Now().Round(0).UTC()
			insertReadyConnectionIntentState(t, ctx, pool, now)
			operation := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "shared-key-000001", []byte(`{}`), now.Add(time.Second))
			createIntent := func() error {
				_, err := store.CreateOrReplayConnectionIntent(ctx, "user-1", "shared-key-000001", "environment-1", now, now.Add(time.Minute), func(context.Context) (*string, error) { return nil, nil }, func() string { return "intent-1" })
				return err
			}
			if test.intentFirst {
				if err := createIntent(); err != nil {
					t.Fatal(err)
				}
				if _, err := store.ReserveRuntimeOperation(ctx, operation); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
					t.Fatalf("Operation collision error = %v", err)
				}
				return
			}
			if _, err := store.ReserveRuntimeOperation(ctx, operation); err != nil {
				t.Fatal(err)
			}
			if err := createIntent(); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
				t.Fatalf("Connection Intent collision error = %v", err)
			}
		})
	}
}

func TestStoreConnectionIntentFinalCheckRejectsActiveStopWithoutMinting(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Round(0).UTC()
	insertReadyConnectionIntentState(t, ctx, pool, now)
	stop := runtimeOperationCandidate(t, "operation-stop", "environment-1", domain.OperationRuntimeStop, "stop-key-0000001", []byte(`{"reason":"manual"}`), now.Add(time.Second))
	if _, err := store.ReserveRuntimeOperation(ctx, stop); err != nil {
		t.Fatal(err)
	}
	mintCalls := 0
	_, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(context.Context) (*string, error) { return nil, nil },
		func() string { mintCalls++; return "intent-unused" },
	)
	if !errors.Is(err, dbstore.ErrOperationConflict) || mintCalls != 0 {
		t.Fatalf("stop race = error:%v mint calls:%d", err, mintCalls)
	}
	var records int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM connection_intent_idempotency`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatalf("persisted Connection Intents = %d, want 0", records)
	}
}

func insertReadyConnectionIntentState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, createdAt time.Time) {
	t.Helper()
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	if _, err := pool.Exec(ctx, `
		UPDATE runtimes
		SET status = 'ready', private_address = '10.0.0.4', boot_id = 'boot-ready', stopped_at = NULL
		WHERE id = 'runtime-1'`); err != nil {
		t.Fatalf("mark Runtime ready: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO environments (
			id, owner_user_id, name, slug, lifecycle, health, region, availability_zone,
			runtime_preset, pinned_profile_version_id, created_at, updated_at, version
		) VALUES ('environment-2', 'user-1', 'dev-2', 'dev-2', 'active', 'healthy', 'us-east-1',
			'us-east-1a', 'standard', 'profile-version-1', $1, $1, 1)`, createdAt); err != nil {
		t.Fatalf("insert second Environment: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auto_stop_policies (id, environment_id, mode, grace_period_seconds) VALUES ('policy-2', 'environment-2', 'manual', 0)`); err != nil {
		t.Fatalf("insert second Auto-stop Policy: %v", err)
	}
}
