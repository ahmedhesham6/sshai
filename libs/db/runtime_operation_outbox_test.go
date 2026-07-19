package db_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreReadsOnlyPendingRuntimeOperationWithPersistedTarget(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 17, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	operation := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, operation); err != nil {
		t.Fatalf("reserve Runtime Operation: %v", err)
	}
	replaceCurrentRuntimeForOutboxTest(t, ctx, pool, createdAt.Add(90*time.Minute))

	dispatch, pending, err := store.PendingRuntimeOperation(ctx, "operation-1")
	if err != nil {
		t.Fatalf("read pending Runtime Operation: %v", err)
	}
	if !pending || dispatch.OperationID != "operation-1" || dispatch.OperationType != domain.OperationRuntimeStart ||
		dispatch.EnvironmentID != "environment-1" || dispatch.RuntimeID != "runtime-1" || dispatch.OwnerUserID != "user-1" || dispatch.StopReason != "" {
		t.Fatalf("pending Runtime Operation = %#v pending:%t", dispatch, pending)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE workflow_outbox
		SET started_at = $2, restate_invocation_id = 'invocation-1'
		WHERE operation_id = $1`, "operation-1", createdAt.Add(2*time.Hour)); err != nil {
		t.Fatalf("mark Runtime Operation outbox started: %v", err)
	}
	if dispatch, pending, err := store.PendingRuntimeOperation(ctx, "operation-1"); err != nil || pending {
		t.Fatalf("started Runtime Operation = %#v pending:%t error:%v", dispatch, pending, err)
	}
}

func replaceCurrentRuntimeForOutboxTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, at time.Time) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin current Runtime replacement: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE runtimes
		SET status = 'absent', retired_at = $2, updated_at = $2
		WHERE id = $1`, "runtime-1", at); err != nil {
		t.Fatalf("retire original Runtime: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runtimes (
			id, environment_id, sequence, status, runtime_preset, region, availability_zone,
			image_version, created_at, updated_at, version
		) VALUES ('runtime-2', 'environment-1', 2, 'absent', 'standard', 'us-east-1',
			'us-east-1a', 'image-2', $1, $1, 1)`, at); err != nil {
		t.Fatalf("insert replacement Runtime: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE environments SET current_runtime_id = 'runtime-2' WHERE id = 'environment-1'`); err != nil {
		t.Fatalf("replace Environment current Runtime: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit current Runtime replacement: %v", err)
	}
}

func TestStoreListsPendingRuntimeOperationsInDeterministicBatches(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 17, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	first := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, first); err != nil {
		t.Fatalf("reserve first Runtime Operation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE operations
		SET status = 'succeeded', restate_invocation_id = 'invocation-1', completed_at = $2
		WHERE id = $1`, "operation-1", createdAt.Add(90*time.Minute)); err != nil {
		t.Fatalf("complete first Runtime Operation projection: %v", err)
	}
	graceStartedAt := createdAt.Add(100 * time.Minute)
	audit := domain.RuntimeStopAuditEvidence{
		Policy:           domain.AutoStopPolicySnapshot{ID: "policy-1", EnvironmentID: "environment-1", Mode: domain.AutoStopWhenFullyIdle, GracePeriodSeconds: 300},
		PolicyGeneration: 2, GraceStartedAt: graceStartedAt, GraceExpiredAt: graceStartedAt.Add(5 * time.Minute), GracePeriodSeconds: 300,
		QualifyingSnapshots: []domain.AutoStopActivitySnapshot{
			{RuntimeID: "runtime-1", Sequence: 4, ObservedAt: graceStartedAt},
			{RuntimeID: "runtime-1", Sequence: 5, ObservedAt: graceStartedAt.Add(5 * time.Minute)},
		},
	}
	stopInput, err := json.Marshal(struct {
		Reason domain.RuntimeStopReason        `json:"reason"`
		Audit  domain.RuntimeStopAuditEvidence `json:"audit"`
	}{Reason: domain.RuntimeStopAutoStop, Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	second := runtimeOperationCandidate(t, "operation-2", "environment-1", domain.OperationRuntimeStop, "request-2", stopInput, createdAt.Add(2*time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, second); err != nil {
		t.Fatalf("reserve second Runtime Operation: %v", err)
	}

	dispatches, err := store.PendingRuntimeOperations(ctx, 1)
	if err != nil {
		t.Fatalf("read first pending Runtime Operation batch: %v", err)
	}
	if len(dispatches) != 1 || dispatches[0].OperationID != "operation-1" {
		t.Fatalf("first pending Runtime Operation batch = %#v", dispatches)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workflow_outbox
		SET started_at = $2, restate_invocation_id = 'invocation-1'
		WHERE operation_id = $1`, "operation-1", createdAt.Add(3*time.Hour)); err != nil {
		t.Fatalf("mark first Runtime Operation outbox started: %v", err)
	}
	dispatches, err = store.PendingRuntimeOperations(ctx, 10)
	if err != nil {
		t.Fatalf("read remaining pending Runtime Operations: %v", err)
	}
	if len(dispatches) != 1 || dispatches[0].OperationID != "operation-2" ||
		dispatches[0].OperationType != domain.OperationRuntimeStop || dispatches[0].RuntimeID != "runtime-1" ||
		dispatches[0].OwnerUserID != "user-1" || dispatches[0].StopReason != domain.RuntimeStopAutoStop ||
		dispatches[0].StopAudit == nil || dispatches[0].StopAudit.PolicyGeneration != 2 || len(dispatches[0].StopAudit.QualifyingSnapshots) != 2 {
		t.Fatalf("remaining pending Runtime Operations = %#v", dispatches)
	}
	if _, err := store.PendingRuntimeOperations(ctx, 0); err == nil {
		t.Fatal("PendingRuntimeOperations(limit 0) error = nil")
	}
}

func TestStoreBacksOffFailedRuntimeOperationSoFreshRowsInterleave(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 17, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	first := runtimeOperationCandidate(t, "operation-poison", "environment-1", domain.OperationRuntimeStart, "request-poison", []byte(`{}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'succeeded', restate_invocation_id = 'invocation-poison', completed_at = $2 WHERE id = $1`, "operation-poison", createdAt.Add(90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	second := runtimeOperationCandidate(t, "operation-fresh", "environment-1", domain.OperationRuntimeStop, "request-fresh", []byte(`{"reason":"manual"}`), createdAt.Add(2*time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, second); err != nil {
		t.Fatal(err)
	}

	attemptedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.DeferRuntimeOperationDispatch(ctx, "operation-poison", attemptedAt); err != nil {
		t.Fatal(err)
	}
	dispatches, err := store.PendingRuntimeOperations(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatches) != 1 || dispatches[0].OperationID != "operation-fresh" {
		t.Fatalf("eligible Runtime Operation = %#v, want fresh row while poison backs off", dispatches)
	}
	var attempts int
	var nextAttemptAt time.Time
	if err := pool.QueryRow(ctx, `SELECT dispatch_attempts, next_attempt_at FROM workflow_outbox WHERE operation_id = 'operation-poison'`).Scan(&attempts, &nextAttemptAt); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !nextAttemptAt.Equal(attemptedAt.Add(5*time.Second)) {
		t.Fatalf("poison backoff = attempts:%d next:%s", attempts, nextAttemptAt)
	}
}
