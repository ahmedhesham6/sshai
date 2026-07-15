package db_test

import (
	"context"
	"database/sql"
	"testing"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
)

func TestEnvironmentStateMigrationEnforcesOwnedCompleteInventory(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertEnvironmentPrerequisites(t, ctx, database, "usr_02", "workos_02", "pro_02", "prv_02")
	insertStateEnvironment(t, ctx, database, "env_01", "usr_01", "prv_01", "first", "op_01", "state-key-1")
	insertStateEnvironment(t, ctx, database, "env_02", "usr_02", "prv_02", "second", "op_02", "state-key-2")

	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin State Component insert: %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	insertProviderResource(t, ctx, transaction, "resource-1", "env_01", "op_01", "vol-1")
	insertStateComponents(t, ctx, transaction, "env_01", "resource-1")
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit exact State Components: %v", err)
	}
	var count int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM state_components WHERE environment_id = 'env_01'`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("State Component count = %d error:%v", count, err)
	}

	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO provider_resources (
			id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata
		) VALUES ('resource-wrong-region', 'env_02', 'op_02', 'aws', 'eu-west-1', 'data_volume', 'vol-2', '{}')`), "23503", "Provider Resource region differs from Environment")
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO provider_resources (
			id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata
		) VALUES ('resource-foreign-operation', 'env_01', 'op_02', 'aws', 'us-east-1', 'data_volume', 'vol-3', '{}')`), "23503", "Provider Resource Operation differs from Environment")
	if _, err := database.ExecContext(ctx, `
		UPDATE operations SET status = 'cancelled', completed_at = now() WHERE id = 'op_01'`); err != nil {
		t.Fatalf("finish creation Operation: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key, input
		) VALUES ('op-runtime', 'env_01', 'runtime.start', 'queued', 'usr_01', 'runtime-key', '{}')`); err != nil {
		t.Fatalf("insert Runtime Operation: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO provider_resources (
			id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata
		) VALUES ('resource-wrong-operation-type', 'env_01', 'op-runtime', 'aws', 'us-east-1', 'data_volume', 'vol-4', '{}')`), "23503", "Provider Resource Operation is not Environment creation")
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO provider_resources (
			id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata
		) VALUES ('resource-duplicate-provider', 'env_02', 'op_02', 'aws', 'us-east-1', 'data_volume', 'vol-1', '{}')`), "23505", "duplicate provider identity")
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE provider_resources SET provider_id = 'vol-mutated' WHERE id = 'resource-1'`), "23514", "mutate Provider Resource identity")
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE provider_resources SET deleted_at = now() WHERE id = 'resource-1'`), "23514", "delete a Provider Resource backing current State")

	assertPostgreSQLCode(t, execError(ctx, database, `
		UPDATE environments SET lifecycle = 'deleted', deleted_at = now(), updated_at = now()
		WHERE id = 'env_01'`), "23514", "delete an Environment retaining current State")

	deletion, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin Environment State deletion: %v", err)
	}
	deletionStatements := []string{
		`UPDATE environments SET lifecycle = 'deleted', deleted_at = now(), updated_at = now() WHERE id = 'env_01'`,
		`DELETE FROM state_components WHERE environment_id = 'env_01'`,
		`UPDATE provider_resources SET deleted_at = now() WHERE id = 'resource-1'`,
	}
	for _, statement := range deletionStatements {
		if _, err := deletion.ExecContext(ctx, statement); err != nil {
			_ = deletion.Rollback()
			t.Fatalf("stage Environment State deletion: %v", err)
		}
	}
	if err := deletion.Commit(); err != nil {
		t.Fatalf("commit complete Environment State deletion: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `DELETE FROM provider_resources WHERE id = 'resource-1'`), "23514", "hard-delete retained Provider Resource history")
}

func TestEnvironmentStateMigrationRejectsPartialOrDriftedComponents(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertStateEnvironment(t, ctx, database, "env_01", "usr_01", "prv_01", "first", "op_01", "state-key-1")

	partial, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin partial State Component insert: %v", err)
	}
	defer func() { _ = partial.Rollback() }()
	insertProviderResource(t, ctx, partial, "resource-1", "env_01", "op_01", "vol-1")
	statements := []string{
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-workspace', 'env_01', 'workspace', 'durable', '/workspace', 'resource-1', 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-home', 'env_01', 'home', 'durable', '/home/dev', 'resource-1', 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-services', 'env_01', 'services', 'durable', '/var/lib/docker', 'resource-1', 'data_volume', 'unknown')`,
	}
	for _, statement := range statements {
		if _, err := partial.ExecContext(ctx, statement); err != nil {
			t.Fatalf("insert partial State Component: %v", err)
		}
	}
	assertPostgreSQLCode(t, partial.Commit(), "23514", "commit partial State Component set")

	complete, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin complete State Component insert: %v", err)
	}
	insertProviderResource(t, ctx, complete, "resource-1", "env_01", "op_01", "vol-1")
	insertStateComponents(t, ctx, complete, "env_01", "resource-1")
	if err := complete.Commit(); err != nil {
		t.Fatalf("commit complete State Component set: %v", err)
	}

	assertPostgreSQLCode(t, execError(ctx, database, `
		UPDATE state_components SET durability = 'durable'
		WHERE environment_id = 'env_01' AND kind = 'cache'`), "23514", "State Component policy drift")
}

func TestEnvironmentStateMigrationRejectsSplitBackends(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertStateEnvironment(t, ctx, database, "env_01", "usr_01", "prv_01", "first", "op_01", "state-key-1")

	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin split State Component insert: %v", err)
	}
	for _, values := range [][2]string{{"resource-1", "vol-1"}, {"resource-2", "vol-2"}} {
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO provider_resources (
				id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata
			) VALUES ($1, 'env_01', 'op_01', 'aws', 'us-east-1', 'data_volume', $2, '{}')`, values[0], values[1]); err != nil {
			_ = transaction.Rollback()
			t.Fatalf("insert split Provider Resource: %v", err)
		}
	}
	statements := []string{
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-workspace', 'env_01', 'workspace', 'durable', '/workspace', 'resource-1', 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-home', 'env_01', 'home', 'durable', '/home/dev', 'resource-1', 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-services', 'env_01', 'services', 'durable', '/var/lib/docker', 'resource-2', 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-cache', 'env_01', 'cache', 'disposable', '/var/cache/devm', 'resource-2', 'data_volume', 'unknown')`,
	}
	for _, statement := range statements {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			_ = transaction.Rollback()
			t.Fatalf("insert split State Component: %v", err)
		}
	}
	assertPostgreSQLCode(t, transaction.Commit(), "23514", "commit State Components split across backends")
}

func TestEnvironmentStateMigrationRejectsComponentsOnDeletedBackend(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertStateEnvironment(t, ctx, database, "env_01", "usr_01", "prv_01", "first", "op_01", "state-key-1")
	if _, err := database.ExecContext(ctx, `
		INSERT INTO provider_resources (
			id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata, deleted_at
		) VALUES ('resource-1', 'env_01', 'op_01', 'aws', 'us-east-1', 'data_volume', 'vol-1', '{}', now())`); err != nil {
		t.Fatalf("insert deleted Provider Resource: %v", err)
	}
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin deleted-backend State Component insert: %v", err)
	}
	insertStateComponents(t, ctx, transaction, "env_01", "resource-1")
	assertPostgreSQLCode(t, transaction.Commit(), "23514", "commit State Components on deleted backend")
}

func TestEnvironmentStateMigrationRejectsActivationWithoutState(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertStateEnvironment(t, ctx, database, "env_01", "usr_01", "prv_01", "first", "op_01", "state-key-1")
	assertPostgreSQLCode(t, execError(ctx, database, `
		UPDATE environments SET lifecycle = 'active', health = 'healthy', updated_at = now(), version = version + 1
		WHERE id = 'env_01'`), "23514", "activate Environment without State")
}

func TestEnvironmentStateMigrationSerializesConcurrentLifecycleAndStateMutation(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertStateEnvironment(t, ctx, database, "env_01", "usr_01", "prv_01", "first", "op_01", "state-key-1")
	initial, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin initial Environment State: %v", err)
	}
	insertProviderResource(t, ctx, initial, "resource-1", "env_01", "op_01", "vol-1")
	insertStateComponents(t, ctx, initial, "env_01", "resource-1")
	if err := initial.Commit(); err != nil {
		t.Fatalf("commit initial Environment State: %v", err)
	}

	removeState, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin State removal: %v", err)
	}
	defer func() { _ = removeState.Rollback() }()
	if _, err := removeState.ExecContext(ctx, `DELETE FROM state_components WHERE environment_id = 'env_01'`); err != nil {
		t.Fatalf("stage State Component removal: %v", err)
	}
	if _, err := removeState.ExecContext(ctx, `UPDATE provider_resources SET deleted_at = now() WHERE id = 'resource-1'`); err != nil {
		t.Fatalf("stage backend retirement: %v", err)
	}

	activate, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin activation: %v", err)
	}
	defer func() { _ = activate.Rollback() }()
	if _, err := activate.ExecContext(ctx, `
		UPDATE environments SET lifecycle = 'active', health = 'healthy', updated_at = now(), version = version + 1
		WHERE id = 'env_01'`); err != nil {
		t.Fatalf("stage activation: %v", err)
	}
	if _, err := activate.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		t.Fatalf("validate activation snapshot: %v", err)
	}

	removalValidated := make(chan error, 1)
	go func() {
		_, err := removeState.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`)
		removalValidated <- err
	}()
	if err := activate.Commit(); err != nil {
		t.Fatalf("commit activation: %v", err)
	}
	assertPostgreSQLCode(t, <-removalValidated, "23514", "validate concurrent State removal after activation")
	if err := removeState.Rollback(); err != nil && err != sql.ErrTxDone {
		t.Fatalf("roll back invalid State removal: %v", err)
	}

	var lifecycle string
	var components, liveBackends int
	if err := database.QueryRowContext(ctx, `SELECT lifecycle FROM environments WHERE id = 'env_01'`).Scan(&lifecycle); err != nil {
		t.Fatalf("read Environment lifecycle: %v", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM state_components WHERE environment_id = 'env_01'`).Scan(&components); err != nil {
		t.Fatalf("count State Components: %v", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM provider_resources WHERE environment_id = 'env_01' AND deleted_at IS NULL`).Scan(&liveBackends); err != nil {
		t.Fatalf("count live backends: %v", err)
	}
	if lifecycle != "active" || components != 4 || liveBackends != 1 {
		t.Fatalf("serialized Environment State = %s/%d/%d", lifecycle, components, liveBackends)
	}
}

func insertStateEnvironment(t *testing.T, ctx context.Context, database *sql.DB, environmentID, ownerID, versionID, slug, operationID, idempotencyKey string) {
	t.Helper()
	if err := insertEnvironment(ctx, database, environmentID, ownerID, versionID, slug); err != nil {
		t.Fatalf("insert Environment: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key, input
		) VALUES ($1, $2, 'environment.create', 'queued', $3, $4, '{}')`, operationID, environmentID, ownerID, idempotencyKey); err != nil {
		t.Fatalf("insert Environment creation Operation: %v", err)
	}
}

type stateExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertProviderResource(t *testing.T, ctx context.Context, database stateExecutor, resourceID, environmentID, operationID, providerID string) {
	t.Helper()
	if _, err := database.ExecContext(ctx, `
		INSERT INTO provider_resources (
			id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata
		) VALUES ($1, $2, $3, 'aws', 'us-east-1', 'data_volume', $4, '{"encrypted":true}')`,
		resourceID, environmentID, operationID, providerID); err != nil {
		t.Fatalf("insert Provider Resource: %v", err)
	}
}

func insertStateComponents(t *testing.T, ctx context.Context, transaction *sql.Tx, environmentID, resourceID string) {
	t.Helper()
	statements := []string{
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-workspace', $1, 'workspace', 'durable', '/workspace', $2, 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-home', $1, 'home', 'durable', '/home/dev', $2, 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-services', $1, 'services', 'durable', '/var/lib/docker', $2, 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('state-cache', $1, 'cache', 'disposable', '/var/cache/devm', $2, 'data_volume', 'unknown')`,
	}
	for _, statement := range statements {
		if _, err := transaction.ExecContext(ctx, statement, environmentID, resourceID); err != nil {
			t.Fatalf("insert State Component: %v", err)
		}
	}
}
