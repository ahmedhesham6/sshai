package db_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestStoreInventoriesEnvironmentStateAtomicallyAndReplaysRecordedIdentity(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}

	reservation := environmentStateReservation(createdAt.Add(time.Minute), "volume-1")
	state, err := store.InventoryEnvironmentState(ctx, "operation-1", reservation)
	if err != nil {
		t.Fatalf("inventory Environment State: %v", err)
	}
	backend := state.Backend()
	if backend.ID != "resource-1" || backend.OperationID != "operation-1" || backend.Provider != "aws" || backend.ProviderID != "volume-1" {
		t.Fatalf("recorded data-volume backend = %#v", backend)
	}
	if components := state.Components(); len(components) != 4 || components[0].Kind != domain.StateWorkspace || components[3].Kind != domain.StateCache {
		t.Fatalf("recorded State Components = %#v", components)
	}

	replay := environmentStateReservation(createdAt.Add(2*time.Minute), "volume-1")
	replay.BackendResourceID = "resource-retry"
	replay.WorkspaceID, replay.HomeID = "workspace-retry", "home-retry"
	replay.ServicesID, replay.CacheID = "services-retry", "cache-retry"
	replay.Metadata = []byte(`{"availabilityZone":"us-east-1a","encrypted":true,"sizeGiB":1e2}`)
	replayed, err := store.InventoryEnvironmentState(ctx, "operation-1", replay)
	if err != nil {
		t.Fatalf("replay Environment State inventory: %v", err)
	}
	if replayed.Backend().ID != backend.ID || !replayed.Backend().CreatedAt.Equal(backend.CreatedAt) {
		t.Fatalf("replay changed recorded backend = %#v", replayed.Backend())
	}
	if replayed.Components()[0].ID != state.Components()[0].ID {
		t.Fatalf("replay changed recorded components = %#v", replayed.Components())
	}

	var resources, components int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources WHERE environment_id = 'environment-1'`).Scan(&resources); err != nil {
		t.Fatalf("count Provider Resources: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_components WHERE environment_id = 'environment-1'`).Scan(&components); err != nil {
		t.Fatalf("count State Components: %v", err)
	}
	if resources != 1 || components != 4 {
		t.Fatalf("recorded Environment State rows = %d/%d", resources, components)
	}
}

func TestStoreRejectsConflictingEnvironmentStateReplay(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", environmentStateReservation(createdAt.Add(time.Minute), "volume-1")); err != nil {
		t.Fatalf("inventory Environment State: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*domain.EnvironmentStateReservation)
	}{
		{name: "provider", mutate: func(input *domain.EnvironmentStateReservation) { input.Provider = "other" }},
		{name: "provider ID", mutate: func(input *domain.EnvironmentStateReservation) { input.ProviderID = "volume-2" }},
		{name: "metadata", mutate: func(input *domain.EnvironmentStateReservation) { input.Metadata = []byte(`{"encrypted":false}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := environmentStateReservation(createdAt.Add(2*time.Minute), "volume-1")
			test.mutate(&input)
			if _, err := store.InventoryEnvironmentState(ctx, "operation-1", input); !errors.Is(err, dbstore.ErrEnvironmentStateConflict) {
				t.Fatalf("conflicting replay error = %v", err)
			}
		})
	}
	var resources, components int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources`).Scan(&resources); err != nil {
		t.Fatalf("count Provider Resources: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_components`).Scan(&components); err != nil {
		t.Fatalf("count State Components: %v", err)
	}
	if resources != 1 || components != 4 {
		t.Fatalf("conflicting replay changed rows = %d/%d", resources, components)
	}
}

func TestStoreSerializesConcurrentEnvironmentStateInventory(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}

	const callers = 8
	states := make([]domain.EnvironmentState, callers)
	errorsByCaller := make([]error, callers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range callers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			input := environmentStateReservation(createdAt.Add(time.Minute), "volume-1")
			suffix := fmt.Sprintf("-%d", index)
			input.BackendResourceID += suffix
			input.WorkspaceID += suffix
			input.HomeID += suffix
			input.ServicesID += suffix
			input.CacheID += suffix
			states[index], errorsByCaller[index] = store.InventoryEnvironmentState(ctx, "operation-1", input)
		}(index)
	}
	close(start)
	wait.Wait()
	backendID := states[0].Backend().ID
	for index := range callers {
		if errorsByCaller[index] != nil {
			t.Fatalf("concurrent inventory %d: %v", index, errorsByCaller[index])
		}
		if states[index].Backend().ID != backendID {
			t.Fatalf("concurrent inventory identities differ: %q != %q", states[index].Backend().ID, backendID)
		}
	}
	var resources, components int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources`).Scan(&resources); err != nil {
		t.Fatalf("count Provider Resources: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_components`).Scan(&components); err != nil {
		t.Fatalf("count State Components: %v", err)
	}
	if resources != 1 || components != 4 {
		t.Fatalf("concurrent Environment State rows = %d/%d", resources, components)
	}
}

func TestStoreRejectsEnvironmentStateForMissingOrTerminalCreation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	reservation := environmentStateReservation(createdAt.Add(time.Minute), "volume-1")
	if _, err := store.InventoryEnvironmentState(ctx, "operation-missing", reservation); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("missing creation Operation error = %v", err)
	} else {
		requirePermanentRepositoryError(t, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE operations SET status = 'failed', completed_at = $2 WHERE id = $1`,
		"operation-1", createdAt.Add(time.Second)); err != nil {
		t.Fatalf("finish creation Operation: %v", err)
	}
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", reservation); err == nil {
		t.Fatal("terminal creation Operation inventoried Environment State")
	} else {
		requirePermanentRepositoryError(t, err)
	}
	var resources, components int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources`).Scan(&resources); err != nil {
		t.Fatalf("count Provider Resources: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_components`).Scan(&components); err != nil {
		t.Fatalf("count State Components: %v", err)
	}
	if resources != 0 || components != 0 {
		t.Fatalf("invalid inventory wrote rows = %d/%d", resources, components)
	}
}

func TestStoreRejectsForeignProviderIdentityAtomically(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	seedForeignEnvironmentState(t, ctx, pool, createdAt.Add(time.Minute), "volume-1", "workspace-foreign")

	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", environmentStateReservation(createdAt.Add(2*time.Minute), "volume-1")); !errors.Is(err, dbstore.ErrEnvironmentStateConflict) {
		t.Fatalf("foreign provider identity error = %v", err)
	} else {
		requirePermanentRepositoryError(t, err)
	}
	var targetResources, targetComponents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources WHERE environment_id = 'environment-1'`).Scan(&targetResources); err != nil {
		t.Fatalf("count target Provider Resources: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_components WHERE environment_id = 'environment-1'`).Scan(&targetComponents); err != nil {
		t.Fatalf("count target State Components: %v", err)
	}
	if targetResources != 0 || targetComponents != 0 {
		t.Fatalf("foreign collision wrote target rows = %d/%d", targetResources, targetComponents)
	}
}

func requirePermanentRepositoryError(t *testing.T, err error) {
	t.Helper()
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("repository error classification = %T %v, want permanent", err, err)
	}
}

func TestStoreRollsBackBackendAfterStateComponentInsertFailure(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	seedForeignEnvironmentState(t, ctx, pool, createdAt.Add(time.Minute), "volume-foreign", "workspace-1")
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", environmentStateReservation(createdAt.Add(2*time.Minute), "volume-target")); err == nil {
		t.Fatal("State Component identity collision error = nil")
	} else {
		requirePermanentRepositoryError(t, err)
	}
	assertTargetEnvironmentStateRows(t, ctx, pool, 0, 0)
}

func TestStoreRollsBackEnvironmentStateOnDeferredCommitFailure(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_test_environment_state_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.environment_id = 'environment-1' THEN
				RAISE EXCEPTION 'test deferred Environment State failure' USING ERRCODE = 'check_violation';
			END IF;
			RETURN NULL;
		END;
		$$`); err != nil {
		t.Fatalf("create deferred failure function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE CONSTRAINT TRIGGER fail_test_environment_state_commit
		AFTER INSERT ON state_components DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION fail_test_environment_state_commit()`); err != nil {
		t.Fatalf("create deferred failure trigger: %v", err)
	}
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", environmentStateReservation(createdAt.Add(time.Minute), "volume-1")); err == nil {
		t.Fatal("deferred Environment State commit error = nil")
	} else {
		requirePermanentRepositoryError(t, err)
	}
	assertTargetEnvironmentStateRows(t, ctx, pool, 0, 0)
}

func TestStoreDistinguishesLargeJSONBIntegersOnEnvironmentStateReplay(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	initial := environmentStateReservation(createdAt.Add(time.Minute), "volume-1")
	initial.Metadata = []byte(`{"value":9007199254740992}`)
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", initial); err != nil {
		t.Fatalf("inventory large-integer metadata: %v", err)
	}
	replay := environmentStateReservation(createdAt.Add(2*time.Minute), "volume-1")
	replay.Metadata = []byte(`{"value":9007199254740993}`)
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", replay); !errors.Is(err, dbstore.ErrEnvironmentStateConflict) {
		t.Fatalf("distinct large-integer replay error = %v", err)
	}
}

func TestStoreSerializesEnvironmentStateInventoryWithCompletion(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 18, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	if _, err := store.RecordEnvironmentCreateInvocation(ctx, "operation-1", "invocation-1", createdAt.Add(time.Second)); err != nil {
		t.Fatalf("record Environment creation invocation: %v", err)
	}

	start := make(chan struct{})
	inventoryResult, completionResult := make(chan error, 1), make(chan error, 1)
	go func() {
		<-start
		_, err := store.InventoryEnvironmentState(ctx, "operation-1", environmentStateReservation(createdAt.Add(30*time.Second), "volume-1"))
		inventoryResult <- err
	}()
	go func() {
		<-start
		_, err := store.CompleteEnvironmentCreation(ctx, "operation-1", createdAt.Add(time.Minute))
		completionResult <- err
	}()
	close(start)
	if err := <-inventoryResult; err != nil {
		t.Fatalf("concurrent Environment State inventory: %v", err)
	}
	if err := <-completionResult; err != nil {
		if !errors.Is(err, dbstore.ErrEnvironmentStateRequired) {
			t.Fatalf("concurrent Environment completion: %v", err)
		}
		if _, err := store.CompleteEnvironmentCreation(ctx, "operation-1", createdAt.Add(time.Minute)); err != nil {
			t.Fatalf("retry Environment completion after inventory: %v", err)
		}
	}
	var lifecycle string
	if err := pool.QueryRow(ctx, `SELECT lifecycle FROM environments WHERE id = 'environment-1'`).Scan(&lifecycle); err != nil {
		t.Fatalf("read completed Environment: %v", err)
	}
	if lifecycle != "active" {
		t.Fatalf("completed Environment lifecycle = %q", lifecycle)
	}
	assertTargetEnvironmentStateRows(t, ctx, pool, 1, 4)
}

func seedForeignEnvironmentState(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Begin(context.Context) (pgx.Tx, error)
}, createdAt time.Time, providerID, workspaceID string) {
	t.Helper()
	statements := []string{
		`INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version) VALUES ('environment-2', 'user-1', 'Other', 'other', 'creating', 'unknown', 'us-east-1', 'us-east-1a', 'standard', 'profile-version-1', 1)`,
		`INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, input) VALUES ('operation-2', 'environment-2', 'environment.create', 'queued', 'user-1', 'request-key-0002', '{}')`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed foreign Environment: %v", err)
		}
	}
	foreign, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin foreign State inventory: %v", err)
	}
	defer func() { _ = foreign.Rollback(ctx) }()
	foreignStatements := []struct {
		statement string
		arguments []any
	}{
		{`INSERT INTO provider_resources (id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata, created_at) VALUES ('resource-foreign', 'environment-2', 'operation-2', 'aws', 'us-east-1', 'data_volume', $2, '{}', $1)`, []any{createdAt, providerID}},
		{`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at) VALUES ($2, 'environment-2', 'workspace', 'durable', '/workspace', 'resource-foreign', 'data_volume', 'unknown', $1, $1)`, []any{createdAt, workspaceID}},
		{`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at) VALUES ('home-foreign', 'environment-2', 'home', 'durable', '/home/dev', 'resource-foreign', 'data_volume', 'unknown', $1, $1)`, []any{createdAt}},
		{`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at) VALUES ('services-foreign', 'environment-2', 'services', 'durable', '/var/lib/docker', 'resource-foreign', 'data_volume', 'unknown', $1, $1)`, []any{createdAt}},
		{`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health, created_at, updated_at) VALUES ('cache-foreign', 'environment-2', 'cache', 'disposable', '/var/cache/devm', 'resource-foreign', 'data_volume', 'unknown', $1, $1)`, []any{createdAt}},
	}
	for _, statement := range foreignStatements {
		if _, err := foreign.Exec(ctx, statement.statement, statement.arguments...); err != nil {
			t.Fatalf("seed foreign State inventory: %v", err)
		}
	}
	if err := foreign.Commit(ctx); err != nil {
		t.Fatalf("commit foreign State inventory: %v", err)
	}
}

func assertTargetEnvironmentStateRows(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, resourcesWant, componentsWant int) {
	t.Helper()
	var resources, components int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources WHERE environment_id = 'environment-1'`).Scan(&resources); err != nil {
		t.Fatalf("count target Provider Resources: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_components WHERE environment_id = 'environment-1'`).Scan(&components); err != nil {
		t.Fatalf("count target State Components: %v", err)
	}
	if resources != resourcesWant || components != componentsWant {
		t.Fatalf("target Environment State rows = %d/%d, want %d/%d", resources, components, resourcesWant, componentsWant)
	}
}

func environmentStateReservation(createdAt time.Time, providerID string) domain.EnvironmentStateReservation {
	return domain.EnvironmentStateReservation{
		WorkspaceID: "workspace-1", HomeID: "home-1", ServicesID: "services-1", CacheID: "cache-1",
		BackendResourceID: "resource-1", Provider: "aws", ProviderID: providerID,
		Metadata: []byte(`{"encrypted":true,"availabilityZone":"us-east-1a","sizeGiB":100}`), CreatedAt: createdAt,
	}
}
