package db_test

import (
	"context"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
)

func TestRuntimeMigrationOwnsOneCurrentRuntimePerEnvironment(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertEnvironmentPrerequisites(t, ctx, database, "usr_02", "workos_02", "pro_02", "prv_02")
	if err := insertEnvironment(ctx, database, "env_01", "usr_01", "prv_01", "first"); err != nil {
		t.Fatalf("insert Environment: %v", err)
	}
	if err := insertEnvironment(ctx, database, "env_02", "usr_02", "prv_02", "second"); err != nil {
		t.Fatalf("insert second Environment: %v", err)
	}

	insertRuntime := func(id, environmentID string, sequence int64) error {
		_, err := database.ExecContext(ctx, `
			INSERT INTO runtimes (
				id, environment_id, sequence, status, runtime_preset, region,
				availability_zone, image_version, version
			) VALUES ($1, $2, $3, 'absent', 'standard', 'us-east-1', 'us-east-1a', 'image-1', 1)`,
			id, environmentID, sequence)
		return err
	}
	if err := insertRuntime("run_01", "env_01", 1); err != nil {
		t.Fatalf("insert Runtime: %v", err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE environments SET current_runtime_id = 'run_01' WHERE id = 'env_01'`); err != nil {
		t.Fatalf("attach current Runtime: %v", err)
	}
	assertPostgreSQLCode(t, insertRuntime("run_02", "env_01", 2), "23505", "second non-retired Runtime")
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE environments SET current_runtime_id = 'run_01' WHERE id = 'env_02'`), "23503", "foreign Environment Runtime")

	replacement, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin Runtime replacement: %v", err)
	}
	defer func() { _ = replacement.Rollback() }()
	if _, err := replacement.ExecContext(ctx, `
		UPDATE runtimes
		SET provider_instance_ref = 'i-old', retired_at = now(), updated_at = now()
		WHERE id = 'run_01'`); err != nil {
		t.Fatalf("retire Runtime: %v", err)
	}
	if _, err := replacement.ExecContext(ctx, `
		INSERT INTO runtimes (
			id, environment_id, sequence, status, runtime_preset, region,
			availability_zone, image_version, version
		) VALUES ('run_02', 'env_01', 2, 'absent', 'standard', 'us-east-1', 'us-east-1a', 'image-1', 1)`); err != nil {
		t.Fatalf("insert replacement Runtime: %v", err)
	}
	if _, err := replacement.ExecContext(ctx, `UPDATE environments SET current_runtime_id = 'run_02' WHERE id = 'env_01'`); err != nil {
		t.Fatalf("replace current Runtime: %v", err)
	}
	if err := replacement.Commit(); err != nil {
		t.Fatalf("commit Runtime replacement: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		UPDATE runtimes SET provider_instance_ref = 'i-new', retired_at = now(), updated_at = now()
		WHERE id = 'run_02'`), "23503", "retire still-current Runtime")

	jump, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin sequence jump: %v", err)
	}
	defer func() { _ = jump.Rollback() }()
	if _, err := jump.ExecContext(ctx, `
		UPDATE runtimes SET provider_instance_ref = 'i-new', retired_at = now(), updated_at = now()
		WHERE id = 'run_02'`); err != nil {
		t.Fatalf("retire Runtime before sequence jump: %v", err)
	}
	_, err = jump.ExecContext(ctx, `
		INSERT INTO runtimes (
			id, environment_id, sequence, status, runtime_preset, region,
			availability_zone, image_version, version
		) VALUES ('run_99', 'env_01', 99, 'absent', 'standard', 'us-east-1', 'us-east-1a', 'image-1', 1)`)
	assertPostgreSQLCode(t, err, "23514", "skipped Runtime sequence")
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE runtimes SET sequence = 3 WHERE id = 'run_02'`), "23514", "mutate Runtime identity")

	if _, err := database.ExecContext(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key, input
		) VALUES ('op_env_02', 'env_02', 'runtime.start', 'queued', 'usr_02', 'runtime-env-2', '{}')`); err != nil {
		t.Fatalf("insert foreign Runtime Operation: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO runtime_operation_targets (operation_id, environment_id, runtime_id, operation_type)
		VALUES ('op_env_02', 'env_01', 'run_01', 'runtime.start')`), "23503", "cross-Environment Runtime target")
	if _, err := database.ExecContext(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key, input
		) VALUES ('op_environment', 'env_01', 'environment.create', 'queued', 'usr_01', 'environment-op', '{}')`); err != nil {
		t.Fatalf("insert non-Runtime Operation: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO runtime_operation_targets (operation_id, environment_id, runtime_id, operation_type)
		VALUES ('op_environment', 'env_01', 'run_01', 'environment.create')`), "23514", "non-Runtime Operation target")
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO workflow_outbox (operation_id, kind, created_at)
		VALUES ('op_env_02', 'runtime.stop', now())`), "23503", "outbox kind differs from Operation type")
}

func TestRuntimeProviderResourceInventoryOwnsProviderIdentity(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	if err := insertEnvironment(ctx, database, "env_01", "usr_01", "prv_01", "first"); err != nil {
		t.Fatalf("insert Environment: %v", err)
	}
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin Runtime inventory: %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	for _, statement := range []string{
		`INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, input)
		 VALUES ('op_01', 'env_01', 'environment.create', 'running', 'usr_01', 'create-1', '{}')`,
		`INSERT INTO runtimes (id, environment_id, sequence, status, runtime_preset, region, availability_zone, image_version, created_at, updated_at, version)
		 VALUES ('run_01', 'env_01', 1, 'absent', 'standard', 'us-east-1', 'us-east-1a', 'image-v1', now(), now(), 1)`,
		`UPDATE environments SET current_runtime_id = 'run_01' WHERE id = 'env_01'`,
		`INSERT INTO provider_resources (id, environment_id, runtime_id, operation_id, provider, region, resource_type, provider_id, metadata)
		 VALUES ('resource-runtime-1', 'env_01', 'run_01', 'op_01', 'aws', 'us-east-1', 'runtime', 'i-runtime-1', '{}')`,
		`UPDATE runtimes SET status = 'provisioning', provider_resource_id = 'resource-runtime-1', updated_at = now(), version = 2 WHERE id = 'run_01'`,
	} {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			t.Fatalf("persist Runtime Provider Resource: %v", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit Runtime Provider Resource: %v", err)
	}
	var providerID, resourceID string
	if err := database.QueryRowContext(ctx, `
		SELECT resource.provider_id, runtime.provider_resource_id
		FROM runtimes runtime
		JOIN provider_resources resource ON resource.id = runtime.provider_resource_id
		WHERE runtime.id = 'run_01'`).Scan(&providerID, &resourceID); err != nil {
		t.Fatalf("read Runtime Provider Resource: %v", err)
	}
	if providerID != "i-runtime-1" || resourceID != "resource-runtime-1" {
		t.Fatalf("Runtime Provider Resource = provider:%q resource:%q", providerID, resourceID)
	}
}

func TestRuntimeMigrationRejectsImpossiblePersistedState(t *testing.T) {
	ctx := context.Background()
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	if err := insertEnvironment(ctx, database, "env_01", "usr_01", "prv_01", "first"); err != nil {
		t.Fatalf("insert Environment: %v", err)
	}

	invalidRuntime := func(id, status string, providerID, privateAddress, bootID, startedAt, stoppedAt any) error {
		_, err := database.ExecContext(ctx, `
			INSERT INTO runtimes (
				id, environment_id, sequence, status, runtime_preset, region, availability_zone,
				image_version, provider_instance_ref, private_address, boot_id, started_at, stopped_at,
				created_at, updated_at, version
			) VALUES ($1, 'env_01', 1, $2, 'standard', 'us-east-1', 'us-east-1a', 'image-1', $3, $4, $5, $6, $7, $8, $9, 1)`,
			id, status, providerID, privateAddress, bootID, startedAt, stoppedAt, createdAt, createdAt.Add(2*time.Minute))
		return err
	}
	assertPostgreSQLCode(t, invalidRuntime("run_ready_without_boot", "ready", "i-1", "10.0.0.4", nil, createdAt.Add(time.Minute), nil), "23514", "ready Runtime without boot")
	assertPostgreSQLCode(t, invalidRuntime("run_absent_with_provider", "absent", "i-1", nil, nil, nil, nil), "23514", "fresh absent Runtime with provider")
	assertPostgreSQLCode(t, invalidRuntime("run_public_route", "ready", "i-1", "203.0.113.1", "boot-1", createdAt.Add(time.Minute), nil), "23514", "ready Runtime with public route")
	assertPostgreSQLCode(t, invalidRuntime("run_cidr_route", "ready", "i-1", "10.0.0.4/24", "boot-1", createdAt.Add(time.Minute), nil), "23514", "ready Runtime with CIDR route")
	assertPostgreSQLCode(t, invalidRuntime(" run_whitespace", "absent", nil, nil, nil, nil, nil), "23514", "whitespace Runtime identity")
	assertPostgreSQLCode(t, invalidRuntime("\trun_tab", "absent", nil, nil, nil, nil, nil), "23514", "tab-padded Runtime identity")
	assertPostgreSQLCode(t, invalidRuntime("run_provider_tab", "provisioning", "i-1\t", nil, nil, nil, nil), "23514", "tab-padded provider identity")
	assertPostgreSQLCode(t, invalidRuntime("run_boot_newline", "ready", "i-1", "10.0.0.4", "boot-1\n", createdAt.Add(time.Minute), nil), "23514", "newline-padded boot identity")
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE environments SET region = E'us-east-1\t' WHERE id = 'env_01'`), "23514", "tab-padded Environment placement")
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO runtimes (
			id, environment_id, sequence, status, runtime_preset, region, availability_zone, image_version, version
		) VALUES ('run_wrong_placement', 'env_01', 1, 'absent', 'standard', 'us-west-2', 'us-east-1a', 'image-1', 1)`), "23503", "Runtime placement differs from Environment")
}
