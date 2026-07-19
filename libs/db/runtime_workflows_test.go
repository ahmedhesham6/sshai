package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStoreRunsRuntimeWorkflowPersistenceLifecycle(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []string{
		`INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key,
			restate_invocation_id, input, created_at, completed_at
		) VALUES ('operation-create', 'environment-1', 'environment.create', 'succeeded',
			'user-1', 'request-create', 'invocation-create', '{}'::jsonb, $1, $1)`,
		`INSERT INTO provider_resources (
			id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata, created_at
		) VALUES ('resource-1', 'environment-1', 'operation-create', 'aws', 'us-east-1',
			'data_volume', 'volume-1', '{}'::jsonb, $1)`,
		`INSERT INTO state_components (
			id, environment_id, kind, durability, mount_path, backend_resource_id,
			backend_resource_type, health, created_at, updated_at
		) VALUES
			('workspace-1', 'environment-1', 'workspace', 'durable', '/workspace', 'resource-1', 'data_volume', 'healthy', $1, $1),
			('home-1', 'environment-1', 'home', 'durable', '/home/dev', 'resource-1', 'data_volume', 'healthy', $1, $1),
			('services-1', 'environment-1', 'services', 'durable', '/var/lib/docker', 'resource-1', 'data_volume', 'healthy', $1, $1),
			('cache-1', 'environment-1', 'cache', 'disposable', '/var/cache/devm', 'resource-1', 'data_volume', 'healthy', $1, $1)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, createdAt); err != nil {
			t.Fatalf("insert Runtime workflow prerequisite: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit Runtime workflow prerequisites: %v", err)
	}

	operation := runtimeOperationCandidate(t, "operation-start", "environment-1", domain.OperationRuntimeStart, "request-start", []byte(`{}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, operation); err != nil {
		t.Fatalf("reserve Runtime start: %v", err)
	}
	dispatch, pending, err := store.PendingRuntimeOperation(ctx, "operation-start")
	if err != nil || !pending {
		t.Fatalf("pending Runtime start = %#v pending:%t error:%v", dispatch, pending, err)
	}
	state, err := store.LoadRuntimeWorkflowOperation(ctx, dispatch, "invocation-start", createdAt.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("load Runtime workflow Operation: %v", err)
	}
	if state.OwnerUserID != "user-1" || state.Runtime.ID != "runtime-1" || state.DataVolumeProviderID != "volume-1" || state.ComputeUsageIntervalID != "" {
		t.Fatalf("Runtime workflow state = %#v", state)
	}
	runtime, err := domain.RestoreRuntime(state.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	starting, err := runtime.BeginStart(createdAt.Add(3 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PersistRuntimeWorkflowTransition(ctx, "operation-start", state.Runtime.Version, starting.Snapshot()); err != nil {
		t.Fatalf("persist Runtime transition: %v", err)
	}
	if err := store.RecordRuntimeWorkflowStep(ctx, "operation-start", "start-decision", "start:image-1", createdAt.Add(3*time.Hour)); err != nil {
		t.Fatalf("record Runtime step: %v", err)
	}
	if err := store.CompleteRuntimeWorkflowOperation(ctx, "operation-start", createdAt.Add(4*time.Hour)); err != nil {
		t.Fatalf("complete Runtime Operation: %v", err)
	}
	var operationStatus, runtimeStatus string
	if err := pool.QueryRow(ctx, `
		SELECT operation.status, runtime.status
		FROM operations operation
		JOIN runtime_operation_targets target ON target.operation_id = operation.id
		JOIN runtimes runtime ON runtime.id = target.runtime_id
		WHERE operation.id = 'operation-start'`).Scan(&operationStatus, &runtimeStatus); err != nil {
		t.Fatal(err)
	}
	if operationStatus != string(domain.OperationSucceeded) || runtimeStatus != string(domain.RuntimeStarting) {
		t.Fatalf("persisted statuses = %q/%q", operationStatus, runtimeStatus)
	}
}
