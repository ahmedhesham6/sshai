package db_test

import (
	"context"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
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

func TestStoreRecordsRuntimeWorkflowFailureCodeAndMessage(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	operation := runtimeOperationCandidate(t, "operation-stop", "environment-1", domain.OperationRuntimeStop, "request-stop", []byte(`{"reason":"manual"}`), createdAt.Add(time.Minute))
	if _, err := store.ReserveRuntimeOperation(ctx, operation); err != nil {
		t.Fatalf("reserve Runtime stop: %v", err)
	}

	const code = "RUNTIME_STOP_FAILED"
	const message = "provider observation did not converge before the durable deadline"
	if err := store.RecordRuntimeWorkflowFailure(ctx, "operation-stop", code, message, createdAt.Add(2*time.Minute)); err != nil {
		t.Fatalf("RecordRuntimeWorkflowFailure(): %v", err)
	}
	var status, storedCode, storedMessage string
	if err := pool.QueryRow(ctx, `SELECT status, error_code, error_message FROM operations WHERE id = 'operation-stop'`).Scan(&status, &storedCode, &storedMessage); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.OperationFailed) || storedCode != code || storedMessage != message {
		t.Fatalf("failure row = status:%q code:%q message:%q", status, storedCode, storedMessage)
	}
}

func TestStorePersistsRuntimeReplacementAndPrunesExpiredProviderResources(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Now().UTC().Truncate(time.Microsecond).Add(-45 * 24 * time.Hour)
	insertRuntimeOperationState(t, ctx, pool, createdAt)
	if _, err := pool.Exec(ctx, `
		INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, restate_invocation_id, input, created_at, completed_at)
		VALUES ('operation-create', 'environment-1', 'environment.create', 'succeeded', 'user-1', 'request-create', 'invocation-create', '{}', $1, $1)`, createdAt); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO provider_resources (id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata, created_at)
		VALUES ('resource-data', 'environment-1', 'operation-create', 'aws', 'us-east-1', 'data_volume', 'volume-1', '{}', $1)`, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at)
		VALUES
		('workspace-1', 'environment-1', 'workspace', 'durable', '/workspace', 'resource-data', 'data_volume', 'healthy', $1, $1),
		('home-1', 'environment-1', 'home', 'durable', '/home/dev', 'resource-data', 'data_volume', 'healthy', $1, $1),
		('services-1', 'environment-1', 'services', 'durable', '/var/lib/docker', 'resource-data', 'data_volume', 'healthy', $1, $1),
		('cache-1', 'environment-1', 'cache', 'disposable', '/var/cache/devm', 'resource-data', 'data_volume', 'healthy', $1, $1)`, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO provider_resources (
			id, environment_id, runtime_id, operation_id, provider, region, resource_type, provider_id, metadata, created_at
		) VALUES
			('resource-runtime-1', 'environment-1', 'runtime-1', 'operation-create', 'aws', 'us-east-1', 'runtime', 'i-runtime-1', '{}', $1),
			('resource-system-1', 'environment-1', 'runtime-1', 'operation-create', 'aws', 'us-east-1', 'system_volume', 'volume-system-1', '{}', $1)`, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE runtimes SET provider_resource_id = 'resource-runtime-1' WHERE id = 'runtime-1'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	operation := runtimeOperationCandidate(t, "operation-replace", "environment-1", domain.OperationRuntimeReplace, "request-replace", []byte(`{}`), createdAt.Add(time.Hour))
	if _, err := store.ReserveRuntimeOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	dispatch, pending, err := store.PendingRuntimeOperation(ctx, "operation-replace")
	if err != nil || !pending {
		t.Fatalf("pending replacement = %#v/%t/%v", dispatch, pending, err)
	}
	state, err := store.LoadRuntimeWorkflowOperation(ctx, dispatch, "invocation-replace", createdAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	old, err := domain.RestoreRuntime(state.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	replacing, err := old.BeginReplacement(createdAt.Add(3 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PersistRuntimeWorkflowTransition(ctx, "operation-replace", state.Runtime.Version, replacing.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistRuntimeWorkflowTransition(ctx, "operation-replace", state.Runtime.Version, replacing.Snapshot()); err != nil {
		t.Fatalf("PersistRuntimeWorkflowTransition(commit-then-lost-response replay): %v", err)
	}
	retiredAt := createdAt.Add(4 * time.Hour)
	retired, err := replacing.Retire(domain.RuntimeStateObservation{ProviderInstanceRef: "i-runtime-1", ExpectedVersion: replacing.Snapshot().Version, ObservedAt: retiredAt})
	if err != nil {
		t.Fatal(err)
	}
	reservation := domain.RuntimeReservation{
		ID: "runtime-2", EnvironmentID: "environment-1", Sequence: 2, RuntimePreset: "standard",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", ImageVersion: "image-2", CreatedAt: retiredAt.Add(time.Minute),
	}
	replacement, err := store.PersistRuntimeReplacement(ctx, "operation-replace", "user-1", replacing.Snapshot().Version, retired.Snapshot(), reservation)
	if err != nil {
		t.Fatalf("PersistRuntimeReplacement(): %v", err)
	}
	replayedReplacement, err := store.PersistRuntimeReplacement(ctx, "operation-replace", "user-1", replacing.Snapshot().Version, retired.Snapshot(), reservation)
	if err != nil || replayedReplacement.ID != replacement.ID || !replayedReplacement.CreatedAt.Equal(replacement.CreatedAt) {
		t.Fatalf("PersistRuntimeReplacement(commit-then-lost-response replay) = %#v/%v", replayedReplacement, err)
	}
	inventory := dbstore.RuntimeProviderResourceInventory{
		RuntimeID: "runtime-2", RuntimeResourceID: "resource-runtime-2", SystemVolumeResourceID: "resource-system-2",
		RuntimeProviderID: "i-runtime-2", SystemVolumeProviderID: "volume-system-2", Provider: "aws", CreatedAt: reservation.CreatedAt,
	}
	if err := store.InventoryReplacementRuntimeResources(ctx, "operation-replace", inventory); err != nil {
		t.Fatalf("InventoryReplacementRuntimeResources(): %v", err)
	}
	if err := store.InventoryReplacementRuntimeResources(ctx, "operation-replace", inventory); err != nil {
		t.Fatalf("InventoryReplacementRuntimeResources(commit-then-lost-response replay): %v", err)
	}
	reserved, err := domain.RestoreRuntime(replacement)
	if err != nil {
		t.Fatal(err)
	}
	provisioned, err := reserved.Provision("i-runtime-2", retiredAt.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PersistReplacementRuntimeTransition(ctx, "operation-replace", replacement.Version, provisioned.Snapshot()); err != nil {
		t.Fatalf("PersistReplacementRuntimeTransition(): %v", err)
	}
	if err := store.PersistReplacementRuntimeTransition(ctx, "operation-replace", replacement.Version, provisioned.Snapshot()); err != nil {
		t.Fatalf("PersistReplacementRuntimeTransition(commit-then-lost-response replay): %v", err)
	}
	var currentRuntimeID, oldStatus, newStatus string
	var runtimeRows, providerRows, retiredProviderRows int
	if err := pool.QueryRow(ctx, `SELECT current_runtime_id FROM environments WHERE id = 'environment-1'`).Scan(&currentRuntimeID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM runtimes WHERE id = 'runtime-1'`).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM runtimes WHERE id = 'runtime-2'`).Scan(&newStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtimes WHERE environment_id = 'environment-1'`).Scan(&runtimeRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources WHERE environment_id = 'environment-1'`).Scan(&providerRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources WHERE runtime_id = 'runtime-1' AND deleted_at = $1`, retiredAt).Scan(&retiredProviderRows); err != nil {
		t.Fatal(err)
	}
	if currentRuntimeID != "runtime-2" || oldStatus != string(domain.RuntimeAbsent) || newStatus != string(domain.RuntimeProvisioning) || runtimeRows != 2 || providerRows != 5 || retiredProviderRows != 2 {
		t.Fatalf("replacement persistence = current:%q old:%q new:%q Runtime rows:%d Provider Resource rows:%d retired Provider Resource rows:%d", currentRuntimeID, oldStatus, newStatus, runtimeRows, providerRows, retiredProviderRows)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET health = 'degraded' WHERE id = 'environment-1'`); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyRuntimeDataVolumeOwnership(ctx, "user-1", "environment-1", "volume-1"); err != nil {
		t.Fatalf("healthy durable State Components in degraded Environment must pass verification: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_resources SET deleted_at = $1 WHERE runtime_id = 'runtime-2'`, createdAt.Add(6*time.Hour)); err != nil {
		t.Fatalf("age current Runtime Provider Resources: %v", err)
	}
	pruned, err := store.PruneProviderResources(ctx, time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneProviderResources(): %v", err)
	}
	var oldResources, currentResources int
	var oldReference, currentReference *string
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources WHERE runtime_id = 'runtime-1'`).Scan(&oldResources); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources WHERE runtime_id = 'runtime-2'`).Scan(&currentResources); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT provider_resource_id FROM runtimes WHERE id = 'runtime-1'`).Scan(&oldReference); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT provider_resource_id FROM runtimes WHERE id = 'runtime-2'`).Scan(&currentReference); err != nil {
		t.Fatal(err)
	}
	if pruned != 2 || oldResources != 0 || currentResources != 2 || oldReference != nil || currentReference == nil || *currentReference != "resource-runtime-2" {
		t.Fatalf("Provider Resource pruning = pruned:%d old:%d current:%d old-ref:%v current-ref:%v", pruned, oldResources, currentResources, oldReference, currentReference)
	}
}
