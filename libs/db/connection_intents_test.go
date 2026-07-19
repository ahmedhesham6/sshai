package db_test

import (
	"context"
	"errors"
	"sync"
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

func TestStoreConsumesOwnedConnectionIntentExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Truncate(time.Microsecond).UTC()
	insertRuntimeOperationState(t, ctx, pool, now)
	operationID := "operation-start"
	start := runtimeOperationCandidate(t, operationID, "environment-1", domain.OperationRuntimeStart, "system:connection-start:consumed", []byte(`{}`), now.Add(time.Second))
	if _, err := store.ReserveRuntimeOperation(ctx, start); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(context.Context) (*string, error) { return &operationID, nil },
		func() string { return "intent-1" },
	)
	if err != nil {
		t.Fatal(err)
	}

	consumed, err := store.ConsumeConnectionIntent(ctx, "workos-1", created.IntentID, "environment-1", now.Add(time.Second))
	if err != nil || consumed.OperationID == nil || *consumed.OperationID != operationID || consumed.UsedAt == nil || !consumed.UsedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("consumed Connection Intent = %#v error:%v", consumed, err)
	}
	if _, err := store.ConsumeConnectionIntent(ctx, "workos-1", created.IntentID, "environment-1", now.Add(2*time.Second)); !errors.Is(err, dbstore.ErrConnectionIntentUsed) {
		t.Fatalf("reused Connection Intent error = %v", err)
	}
	if _, err := store.ConsumeConnectionIntent(ctx, "workos-2", created.IntentID, "environment-1", now.Add(2*time.Second)); !errors.Is(err, dbstore.ErrConnectionIntentNotFound) {
		t.Fatalf("foreign Connection Intent error = %v", err)
	}
	if _, err := store.ConsumeConnectionIntent(ctx, "workos-1", created.IntentID, "environment-2", now.Add(2*time.Second)); !errors.Is(err, dbstore.ErrConnectionIntentNotFound) {
		t.Fatalf("wrong-Environment Connection Intent error = %v", err)
	}
}

func TestStoreConsumeConnectionIntentClampsProxyClockBeforeCreation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Now().Truncate(time.Microsecond).UTC()
	insertReadyConnectionIntentState(t, ctx, pool, createdAt)
	created, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", createdAt, createdAt.Add(time.Minute),
		func(context.Context) (*string, error) { return nil, nil }, func() string { return "intent-1" },
	)
	if err != nil {
		t.Fatal(err)
	}
	var intentCreatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM connection_intent_idempotency WHERE intent_id = $1`, created.IntentID).Scan(&intentCreatedAt); err != nil {
		t.Fatal(err)
	}
	consumed, err := store.ConsumeConnectionIntent(ctx, "workos-1", created.IntentID, "environment-1", createdAt.Add(-time.Minute))
	if err != nil || consumed.UsedAt == nil || !consumed.UsedAt.Equal(intentCreatedAt) {
		t.Fatalf("clock-skew consume = %#v error:%v", consumed, err)
	}
}

func TestStoreConsumeConnectionIntentClassifiesCheckViolation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Truncate(time.Microsecond).UTC()
	insertReadyConnectionIntentState(t, ctx, pool, now)
	created, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(context.Context) (*string, error) { return nil, nil }, func() string { return "intent-1" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		ALTER TABLE connection_intent_idempotency
		DROP CONSTRAINT connection_intent_used_before_expiry_check,
		ADD CONSTRAINT connection_intent_used_before_expiry_check CHECK (used_at IS NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeConnectionIntent(ctx, "workos-1", created.IntentID, "environment-1", now.Add(time.Second)); !errors.Is(err, dbstore.ErrConnectionIntentInvariant) {
		t.Fatalf("CHECK violation classification = %v", err)
	}
}

func TestStoreConnectionIntentRejectsPreparedStartThatTurnsTerminalBeforePersist(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Truncate(time.Microsecond).UTC()
	insertRuntimeOperationState(t, ctx, pool, now)
	mintCalls := 0
	_, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(prepareCtx context.Context) (*string, error) {
			start := runtimeOperationCandidate(t, "operation-start", "environment-1", domain.OperationRuntimeStart, "system:connection-start:terminal", []byte(`{}`), now.Add(time.Second))
			command, reserveErr := store.ReserveRuntimeOperation(prepareCtx, start)
			if reserveErr != nil {
				return nil, reserveErr
			}
			operationID := command.Operation().Snapshot().ID
			if _, updateErr := pool.Exec(prepareCtx, `
				UPDATE operations
				SET status = 'failed', error_code = 'START_FAILED', error_message = 'failed', completed_at = $2
				WHERE id = $1`, operationID, now.Add(2*time.Second)); updateErr != nil {
				return nil, updateErr
			}
			return &operationID, nil
		},
		func() string { mintCalls++; return "intent-unused" },
	)
	if !errors.Is(err, dbstore.ErrOperationConflict) || mintCalls != 0 {
		t.Fatalf("terminal prepared start = error:%v mint calls:%d", err, mintCalls)
	}
}

func TestStoreConnectionIntentLocksPreparedStartThroughMintAndCommit(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Truncate(time.Microsecond).UTC()
	insertRuntimeOperationState(t, ctx, pool, now)
	start := runtimeOperationCandidate(t, "operation-start", "environment-1", domain.OperationRuntimeStart, "system:connection-start:locked", []byte(`{}`), now.Add(time.Second))
	command, err := store.ReserveRuntimeOperation(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	operationID := command.Operation().Snapshot().ID
	updateStarted := make(chan struct{})
	updateFinished := make(chan error, 1)
	created, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(context.Context) (*string, error) { return &operationID, nil },
		func() string {
			go func() {
				close(updateStarted)
				_, updateErr := pool.Exec(ctx, `
					UPDATE operations
					SET status = 'failed', error_code = 'START_FAILED', error_message = 'failed', completed_at = $2
					WHERE id = $1`, operationID, now.Add(2*time.Second))
				updateFinished <- updateErr
			}()
			<-updateStarted
			select {
			case updateErr := <-updateFinished:
				t.Fatalf("prepared Operation terminalized before Intent commit: %v", updateErr)
			case <-time.After(50 * time.Millisecond):
			}
			return "intent-1"
		},
	)
	if err != nil || created.OperationID == nil || *created.OperationID != operationID {
		t.Fatalf("created Connection Intent = %#v error:%v", created, err)
	}
	select {
	case updateErr := <-updateFinished:
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal Operation update remained blocked after Intent commit")
	}
}

func TestStoreConnectionIntentRejectsReadyRuntimeThatStopsBeforeLockedPersist(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Truncate(time.Microsecond).UTC()
	insertReadyConnectionIntentState(t, ctx, pool, now)
	mintCalls := 0
	_, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(prepareCtx context.Context) (*string, error) {
			stoppedAt := now.Add(3 * time.Minute)
			_, updateErr := pool.Exec(prepareCtx, `
				UPDATE runtimes
				SET status = 'stopped', private_address = NULL, boot_id = NULL,
				    stopped_at = $2, updated_at = $2, version = version + 1
				WHERE id = $1`, "runtime-1", stoppedAt)
			return nil, updateErr
		},
		func() string { mintCalls++; return "intent-unused" },
	)
	if !errors.Is(err, domain.ErrRuntimeCommandState) || mintCalls != 0 {
		t.Fatalf("ready-to-stopped window = error:%v mint calls:%d", err, mintCalls)
	}
}

func TestStoreConnectionIntentLocksReadyRuntimeThroughMintAndCommit(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Truncate(time.Microsecond).UTC()
	insertReadyConnectionIntentState(t, ctx, pool, now)
	updateStarted := make(chan struct{})
	updateFinished := make(chan error, 1)
	created, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(context.Context) (*string, error) { return nil, nil },
		func() string {
			go func() {
				close(updateStarted)
				stoppedAt := now.Add(3 * time.Minute)
				_, updateErr := pool.Exec(ctx, `
					UPDATE runtimes
					SET status = 'stopped', private_address = NULL, boot_id = NULL,
					    stopped_at = $2, updated_at = $2, version = version + 1
					WHERE id = $1`, "runtime-1", stoppedAt)
				updateFinished <- updateErr
			}()
			<-updateStarted
			select {
			case updateErr := <-updateFinished:
				t.Fatalf("ready Runtime stopped before Intent commit: %v", updateErr)
			case <-time.After(50 * time.Millisecond):
			}
			return "intent-1"
		},
	)
	if err != nil || created.OperationID != nil {
		t.Fatalf("created ready-runtime Intent = %#v error:%v", created, err)
	}
	select {
	case updateErr := <-updateFinished:
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Runtime transition remained blocked after Intent commit")
	}
}

func TestStoreRejectsExpiredConnectionIntentWithoutConsumingIt(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Now().Truncate(time.Microsecond).UTC()
	insertReadyConnectionIntentState(t, ctx, pool, now)
	created, err := store.CreateOrReplayConnectionIntent(
		ctx, "user-1", "connection-key-0001", "environment-1", now, now.Add(time.Minute),
		func(context.Context) (*string, error) { return nil, nil }, func() string { return "intent-1" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeConnectionIntent(ctx, "workos-1", created.IntentID, "environment-1", created.ExpiresAt); !errors.Is(err, dbstore.ErrConnectionIntentExpired) {
		t.Fatalf("expired Connection Intent error = %v", err)
	}
	var usedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT used_at FROM connection_intent_idempotency WHERE intent_id = $1`, created.IntentID).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if usedAt != nil {
		t.Fatalf("expired Connection Intent used_at = %s", usedAt)
	}
}

func TestStoreConcurrentColdConnectionIntentsDoNotStarveTwoConnectionPool(t *testing.T) {
	setupCtx := context.Background()
	database, connectionString := openTestDatabase(t, setupCtx)
	if err := dbstore.Migrate(setupCtx, database); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(connectionString)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(setupCtx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := dbstore.NewStore(pool)
	now := time.Now().Truncate(time.Microsecond).UTC()
	insertRuntimeOperationState(t, setupCtx, pool, now)
	ctx, cancel := context.WithTimeout(setupCtx, 3*time.Second)
	defer cancel()

	var ready sync.WaitGroup
	ready.Add(2)
	release := make(chan struct{})
	go func() {
		ready.Wait()
		close(release)
	}()
	results := make(chan error, 2)
	for index := range 2 {
		go func() {
			prepare := func(prepareCtx context.Context) (*string, error) {
				ready.Done()
				select {
				case <-release:
				case <-prepareCtx.Done():
					return nil, context.Cause(prepareCtx)
				}
				operationID := "operation-start-" + string(rune('1'+index))
				operation := runtimeOperationCandidate(t, operationID, "environment-1", domain.OperationRuntimeStart, "system:connection-start:"+operationID, []byte(`{}`), now.Add(time.Second))
				command, reserveErr := store.ReserveRuntimeOperation(prepareCtx, operation)
				if reserveErr == nil {
					reservedID := command.Operation().Snapshot().ID
					return &reservedID, nil
				}
				if !errors.Is(reserveErr, dbstore.ErrOperationConflict) {
					return nil, reserveErr
				}
				var activeID string
				if queryErr := pool.QueryRow(prepareCtx, `
					SELECT id FROM operations
					WHERE environment_id = 'environment-1' AND status IN ('queued', 'running')`,
				).Scan(&activeID); queryErr != nil {
					return nil, queryErr
				}
				return &activeID, nil
			}
			_, createErr := store.CreateOrReplayConnectionIntent(
				ctx, "user-1", "connection-key-000"+string(rune('1'+index)), "environment-1",
				now, now.Add(time.Minute), prepare, func() string { return "intent-" + string(rune('1'+index)) },
			)
			results <- createErr
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent cold Connection Intent: %v", err)
		}
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
