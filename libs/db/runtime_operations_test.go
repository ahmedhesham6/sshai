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

func TestStoreReservesRuntimeOperationWithExactIdempotencyIdentity(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	candidate := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour))

	reserved, err := store.ReserveRuntimeOperation(ctx, candidate)
	if err != nil {
		t.Fatalf("ReserveRuntimeOperation(): %v", err)
	}
	if got := reserved.Operation().Snapshot(); got.ID != "operation-1" || got.Type != domain.OperationRuntimeStart || got.Status != domain.OperationQueued {
		t.Fatalf("reserved Operation = %#v", got)
	}
	if got := reserved.Runtime().Snapshot(); got.ID != "runtime-1" || got.Status != domain.RuntimeStopped {
		t.Fatalf("reserved Runtime = %#v", got)
	}

	replay := runtimeOperationCandidate(t, "operation-unused", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(2*time.Hour))
	replayed, err := store.ReserveRuntimeOperation(ctx, replay)
	if err != nil || replayed.Operation().Snapshot().ID != "operation-1" {
		t.Fatalf("replay = %#v error:%v", replayed.Operation().Snapshot(), err)
	}
	conflicts := []domain.Operation{
		runtimeOperationCandidate(t, "operation-type", "environment-1", domain.OperationRuntimeReplace, "request-1", []byte(`{}`), createdAt.Add(2*time.Hour)),
		runtimeOperationCandidate(t, "operation-environment", "environment-2", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(2*time.Hour)),
		runtimeOperationCandidate(t, "operation-input", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{"different":true}`), createdAt.Add(2*time.Hour)),
		runtimeOperationCandidate(t, "operation-large-integer", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{"value":9007199254740993}`), createdAt.Add(2*time.Hour)),
	}
	for _, conflict := range conflicts {
		if _, err := store.ReserveRuntimeOperation(ctx, conflict); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
			t.Fatalf("conflicting replay error = %v", err)
		}
	}
	var operationCount, outboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE type = 'runtime.start'`).Scan(&operationCount); err != nil {
		t.Fatalf("count Runtime Operations: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_outbox WHERE kind = 'runtime.start'`).Scan(&outboxCount); err != nil {
		t.Fatalf("count Runtime outbox: %v", err)
	}
	if operationCount != 1 || outboxCount != 1 {
		t.Fatalf("persisted Runtime Operation/outbox = %d/%d", operationCount, outboxCount)
	}
	var targetRuntimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM runtime_operation_targets WHERE operation_id = 'operation-1'`).Scan(&targetRuntimeID); err != nil || targetRuntimeID != "runtime-1" {
		t.Fatalf("Runtime Operation target = %q error:%v", targetRuntimeID, err)
	}
}

func TestStoreHistoricalRuntimeOperationReplayKeepsOriginalTarget(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	candidate := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, candidate); err != nil {
		t.Fatalf("reserve Runtime Operation: %v", err)
	}
	completedAt := createdAt.Add(2 * time.Hour)
	if _, err := pool.Exec(ctx, `
		UPDATE operations
		SET status = 'succeeded', restate_invocation_id = 'invocation-1', completed_at = $1
		WHERE id = 'operation-1'`, completedAt); err != nil {
		t.Fatalf("complete Runtime Operation: %v", err)
	}
	replacement, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin Runtime replacement: %v", err)
	}
	defer func() { _ = replacement.Rollback(ctx) }()
	retiredAt := createdAt.Add(3 * time.Hour)
	if _, err := replacement.Exec(ctx, `
		UPDATE runtimes
		SET status = 'absent', retired_at = $2, updated_at = $2
		WHERE id = $1`, "runtime-1", retiredAt); err != nil {
		t.Fatalf("retire Runtime: %v", err)
	}
	if _, err := replacement.Exec(ctx, `
		INSERT INTO runtimes (
			id, environment_id, sequence, status, runtime_preset, region, availability_zone,
			image_version, created_at, updated_at, version
		) VALUES ('runtime-2', 'environment-1', 2, 'absent', 'standard', 'us-east-1',
			'us-east-1a', 'image-2', $1, $1, 1)`, retiredAt); err != nil {
		t.Fatalf("insert replacement Runtime: %v", err)
	}
	if _, err := replacement.Exec(ctx, `UPDATE environments SET current_runtime_id = 'runtime-2' WHERE id = 'environment-1'`); err != nil {
		t.Fatalf("replace current Runtime: %v", err)
	}
	if err := replacement.Commit(ctx); err != nil {
		t.Fatalf("commit Runtime replacement: %v", err)
	}

	replayed, err := store.ReserveRuntimeOperation(ctx, runtimeOperationCandidate(t, "operation-unused", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(4*time.Hour)))
	if err != nil {
		t.Fatalf("replay historical Runtime Operation: %v", err)
	}
	if got := replayed.Runtime().Snapshot().ID; got != "runtime-1" {
		t.Fatalf("historical target Runtime = %q", got)
	}
	if got := replayed.Environment().Snapshot().CurrentRuntimeID; got == nil || *got != "runtime-2" {
		t.Fatalf("current Environment Runtime = %v", got)
	}
}

func TestStoreRuntimeOperationIdempotencyReplayIgnoresCurrentLifecycle(t *testing.T) {
	tests := []struct {
		name      string
		lifecycle domain.EnvironmentLifecycle
	}{
		{name: "active Environment", lifecycle: domain.EnvironmentActive},
		{name: "deleting Environment", lifecycle: domain.EnvironmentDeleting},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, pool := openTestStoreAndPool(t, ctx)
			createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
			insertRuntimeOperationState(t, ctx, pool, createdAt)
			candidate := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour))
			if _, err := store.ReserveRuntimeOperation(ctx, candidate); err != nil {
				t.Fatalf("reserve Runtime Operation: %v", err)
			}
			completedAt := createdAt.Add(2 * time.Hour)
			if _, err := pool.Exec(ctx, `
				UPDATE operations
				SET status = 'succeeded', restate_invocation_id = 'invocation-1', completed_at = $1
				WHERE id = 'operation-1'`, completedAt); err != nil {
				t.Fatalf("complete Runtime Operation: %v", err)
			}
			if test.lifecycle != domain.EnvironmentActive {
				insertRuntimeOperationEnvironmentState(t, ctx, pool, createdAt)
				if _, err := pool.Exec(ctx, `
					UPDATE environments
					SET lifecycle = $1, updated_at = $2, version = version + 1
					WHERE id = 'environment-1'`, string(test.lifecycle), completedAt.Add(time.Minute)); err != nil {
					t.Fatalf("move Environment to %q: %v", test.lifecycle, err)
				}
			}

			replayed, err := store.ReserveRuntimeOperation(ctx, runtimeOperationCandidate(
				t, "operation-unused", "environment-1", domain.OperationRuntimeStart,
				"request-1", []byte(`{}`), completedAt.Add(time.Hour),
			))
			if err != nil {
				t.Fatalf("replay historical Runtime Operation: %v", err)
			}
			if got := replayed.Operation().Snapshot().ID; got != "operation-1" {
				t.Fatalf("replayed Operation ID = %q, want historical Operation", got)
			}
			if got := replayed.Environment().Snapshot().Lifecycle; got != test.lifecycle {
				t.Fatalf("current Environment lifecycle = %q, want %q", got, test.lifecycle)
			}
		})
	}
}

func insertRuntimeOperationEnvironmentState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, createdAt time.Time) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin Environment State inventory: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []string{
		`INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, restate_invocation_id, input, created_at, completed_at)
		 VALUES ('operation-create-state', 'environment-1', 'environment.create', 'succeeded', 'user-1', 'request-create-state', 'invocation-create-state', '{}', $1, $1)`,
		`INSERT INTO provider_resources (id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata, created_at)
		 VALUES ('resource-state', 'environment-1', 'operation-create-state', 'aws', 'us-east-1', 'data_volume', 'volume-state', '{}', $1)`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at)
		 VALUES ('state-workspace', 'environment-1', 'workspace', 'durable', '/workspace', 'resource-state', 'data_volume', 'healthy', $1, $1)`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at)
		 VALUES ('state-home', 'environment-1', 'home', 'durable', '/home/dev', 'resource-state', 'data_volume', 'healthy', $1, $1)`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at)
		 VALUES ('state-services', 'environment-1', 'services', 'durable', '/var/lib/docker', 'resource-state', 'data_volume', 'healthy', $1, $1)`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at)
		 VALUES ('state-cache', 'environment-1', 'cache', 'disposable', '/var/cache/devm', 'resource-state', 'data_volume', 'healthy', $1, $1)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, createdAt); err != nil {
			t.Fatalf("inventory Environment State: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit Environment State inventory: %v", err)
	}
}

func TestStoreSerializesConcurrentRuntimeOperationReservation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	candidates := []domain.Operation{
		runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour)),
		runtimeOperationCandidate(t, "operation-2", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(2*time.Hour)),
	}
	type result struct {
		id  string
		err error
	}
	start := make(chan struct{})
	results := make(chan result, len(candidates))
	for _, candidate := range candidates {
		go func() {
			<-start
			command, err := store.ReserveRuntimeOperation(ctx, candidate)
			results <- result{id: command.Operation().Snapshot().ID, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.id == "" || first.id != second.id {
		t.Fatalf("concurrent reservations = %#v, %#v", first, second)
	}
	var operationCount, outboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE type = 'runtime.start'`).Scan(&operationCount); err != nil {
		t.Fatalf("count Runtime Operations: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_outbox WHERE kind = 'runtime.start'`).Scan(&outboxCount); err != nil {
		t.Fatalf("count Runtime outbox: %v", err)
	}
	if operationCount != 1 || outboxCount != 1 {
		t.Fatalf("persisted Runtime Operation/outbox = %d/%d", operationCount, outboxCount)
	}
}

func TestStoreSerializesConcurrentRuntimeOperationConflicts(t *testing.T) {
	tests := []struct {
		name      string
		secondKey string
		second    []byte
		want      error
	}{
		{name: "same key different input", secondKey: "request-1", second: []byte(`{"different":true}`), want: dbstore.ErrIdempotencyConflict},
		{name: "different key active Operation", secondKey: "request-2", second: []byte(`{}`), want: dbstore.ErrOperationConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, pool := openTestStoreAndPool(t, ctx)
			createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
			insertRuntimeOperationState(t, ctx, pool, createdAt)
			candidates := []domain.Operation{
				runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour)),
				runtimeOperationCandidate(t, "operation-2", "environment-1", domain.OperationRuntimeStart, test.secondKey, test.second, createdAt.Add(2*time.Hour)),
			}
			start := make(chan struct{})
			results := make(chan error, len(candidates))
			for _, candidate := range candidates {
				go func() {
					<-start
					_, err := store.ReserveRuntimeOperation(ctx, candidate)
					results <- err
				}()
			}
			close(start)
			first, second := <-results, <-results
			if (first == nil && errors.Is(second, test.want)) || (second == nil && errors.Is(first, test.want)) {
				return
			}
			t.Fatalf("concurrent conflict errors = %v, %v; want one nil and one %v", first, second, test.want)
		})
	}
}

func TestStoreSerializesEnvironmentCreateAndRuntimeOperationIdempotency(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 19, 14, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt.Add(-time.Hour))
	idempotencyKey := "shared-create-runtime-key-0001"
	creation := newEnvironmentCreationWithSeed(
		t, "environment-2", "policy-2", "operation-create", "project-seed-1", "workspace",
		idempotencyKey, []byte(`{"name":"workspace"}`), createdAt,
	)
	runtimeOperation := runtimeOperationCandidate(
		t, "operation-runtime", "environment-1", domain.OperationRuntimeStart,
		idempotencyKey, []byte(`{}`), createdAt,
	)

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := store.ReserveEnvironmentCreation(ctx, creation)
		results <- err
	}()
	go func() {
		<-start
		_, err := store.ReserveRuntimeOperation(ctx, runtimeOperation)
		results <- err
	}()
	close(start)
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, dbstore.ErrIdempotencyConflict)) ||
		(second == nil && errors.Is(first, dbstore.ErrIdempotencyConflict))) {
		t.Fatalf("concurrent create and Runtime command = %v, %v; want one success and one idempotency conflict", first, second)
	}

	var operationCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM operations
		WHERE requested_by_user_id = 'user-1' AND idempotency_key = $1`, idempotencyKey).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if operationCount != 1 {
		t.Fatalf("Operations with shared idempotency key = %d, want 1", operationCount)
	}
}

func TestStoreDoesNotCollapseDistinctLargeJSONIntegers(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	first := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{"value":9007199254740992}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, first); err != nil {
		t.Fatalf("reserve first Runtime Operation: %v", err)
	}
	conflict := runtimeOperationCandidate(t, "operation-2", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{"value":9007199254740993}`), createdAt.Add(2*time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, conflict); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("large integer conflict = %v", err)
	}
}

func TestStoreRejectsConflictingOrForeignRuntimeOperation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	first := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, first); err != nil {
		t.Fatalf("reserve first Runtime Operation: %v", err)
	}
	conflicting := runtimeOperationCandidate(t, "operation-2", "environment-1", domain.OperationRuntimeStop, "request-2", []byte(`{"reason":"manual"}`), createdAt.Add(2*time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, conflicting); !errors.Is(err, dbstore.ErrOperationConflict) {
		t.Fatalf("active Operation conflict = %v", err)
	}
	foreign, err := domain.QueueOperation(domain.OperationRequest{
		ID: "operation-foreign", EnvironmentID: "environment-1", Type: domain.OperationRuntimeStart,
		RequestedByUserID: "user-2", IdempotencyKey: "request-foreign", Input: []byte(`{}`),
		CreatedAt: createdAt.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueueOperation(): %v", err)
	}
	if _, err := store.ReserveRuntimeOperation(ctx, foreign); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign Runtime Operation = %v", err)
	}
}

func TestStoreRollsBackRuntimeOperationWhenOutboxInsertFails(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_runtime_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.kind = 'runtime.start' THEN
				RAISE EXCEPTION 'forced outbox failure';
			END IF;
			RETURN NEW;
		END;
		$$`); err != nil {
		t.Fatalf("create outbox failure function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER fail_runtime_outbox
		BEFORE INSERT ON workflow_outbox
		FOR EACH ROW EXECUTE FUNCTION fail_runtime_outbox()`); err != nil {
		t.Fatalf("create outbox failure trigger: %v", err)
	}
	candidate := runtimeOperationCandidate(t, "operation-1", "environment-1", domain.OperationRuntimeStart, "request-1", []byte(`{}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, candidate); err == nil {
		t.Fatal("ReserveRuntimeOperation() error = nil")
	}
	var operations, targets, outbox int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM operations WHERE id = 'operation-1'),
			(SELECT count(*) FROM runtime_operation_targets WHERE operation_id = 'operation-1'),
			(SELECT count(*) FROM workflow_outbox WHERE operation_id = 'operation-1')`).Scan(&operations, &targets, &outbox); err != nil {
		t.Fatalf("count rolled-back Runtime Operation: %v", err)
	}
	if operations != 0 || targets != 0 || outbox != 0 {
		t.Fatalf("rolled-back Runtime Operation rows = operation:%d target:%d outbox:%d", operations, targets, outbox)
	}
}

func runtimeOperationCandidate(t *testing.T, id, environmentID string, operationType domain.OperationType, key string, input []byte, createdAt time.Time) domain.Operation {
	t.Helper()
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: id, EnvironmentID: environmentID, Type: operationType, RequestedByUserID: "user-1",
		IdempotencyKey: key, Input: input, CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("QueueOperation(): %v", err)
	}
	return operation
}

func insertRuntimeOperationState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, createdAt time.Time) {
	t.Helper()
	insertCreationPrerequisites(t, ctx, pool)
	statements := []struct {
		query string
		args  []any
	}{
		{query: `
			INSERT INTO environments (
				id, owner_user_id, name, slug, lifecycle, health, region, availability_zone,
				runtime_preset, pinned_profile_version_id, created_at, updated_at, version
			) VALUES ('environment-1', 'user-1', 'dev', 'dev', 'active', 'healthy', 'us-east-1',
				'us-east-1a', 'standard', 'profile-version-1', $1, $1, 1)`, args: []any{createdAt}},
		{query: `INSERT INTO auto_stop_policies (id, environment_id, mode, grace_period_seconds) VALUES ('policy-1', 'environment-1', 'manual', 0)`},
		{query: `
			INSERT INTO runtimes (
				id, environment_id, sequence, status, runtime_preset, region, availability_zone,
				image_version, provider_instance_ref, started_at, stopped_at, created_at, updated_at, version
			) VALUES ('runtime-1', 'environment-1', 1, 'stopped', 'standard', 'us-east-1', 'us-east-1a',
				'image-1', 'i-runtime-1', $2, $3, $1, $3, 5)`, args: []any{createdAt, createdAt.Add(time.Minute), createdAt.Add(2 * time.Minute)}},
		{query: `UPDATE environments SET current_runtime_id = 'runtime-1' WHERE id = 'environment-1'`},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("insert Runtime operation state: %v", err)
		}
	}
}
