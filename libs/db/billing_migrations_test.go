package db_test

import (
	"context"
	"testing"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
)

func TestMigrationsKeepCreditRateHistoryImmutable(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO credit_rates (
			version, resource_type, region, runtime_preset, raw_unit,
			credits_per_unit, effective_at
		) VALUES ('compute-us-east-1-small-v1', 'compute', 'us-east-1', 'small', 'second', '2', now())`); err != nil {
		t.Fatalf("insert Credit Rate: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		UPDATE credit_rates SET credits_per_unit = '3'
		WHERE version = 'compute-us-east-1-small-v1'`), "23514", "rewrite Credit Rate history")
	assertPostgreSQLCode(t, execError(ctx, database, `
		DELETE FROM credit_rates
		WHERE version = 'compute-us-east-1-small-v1'`), "23514", "delete Credit Rate history")
}

func TestMigrationsRejectAmbiguousCreditRateEffectiveKeys(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO credit_rates (
			version, resource_type, region, runtime_preset, raw_unit,
			credits_per_unit, effective_at
		) VALUES ('compute-v1', 'compute', 'us-east-1', 'small', 'second', '2', '2026-07-13T00:00:00Z')`); err != nil {
		t.Fatalf("insert first Credit Rate: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO credit_rates (
			version, resource_type, region, runtime_preset, raw_unit,
			credits_per_unit, effective_at
		) VALUES ('compute-v2', 'compute', 'us-east-1', 'small', 'second', '3', '2026-07-13T00:00:00Z')`), "23505", "ambiguous Credit Rate effective key")
}

func TestMigrationsAllowOneOpenComputeIntervalPerEnvironmentAndRuntime(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	if err := insertEnvironment(ctx, database, "env_01", "usr_01", "prv_01", "workspace"); err != nil {
		t.Fatalf("insert Environment: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO compute_usage_intervals (
			id, user_id, environment_id, runtime_id, region, runtime_preset, started_at
		) VALUES ('cui_01', 'usr_01', 'env_01', 'run_01', 'us-east-1', 'small', now())`); err != nil {
		t.Fatalf("insert first Compute Usage Interval: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO compute_usage_intervals (
			id, user_id, environment_id, runtime_id, region, runtime_preset, started_at
		) VALUES ('cui_02', 'usr_01', 'env_01', 'run_02', 'us-east-1', 'small', now())`), "23505", "second open Environment interval")
}

func TestMigrationsKeepCreditTransactionsImmutable(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertUser(t, ctx, database, "usr_01", "workos_01")
	if _, err := database.ExecContext(ctx, `
		INSERT INTO credit_transactions (
			id, user_id, kind, credits, idempotency_key, occurred_at, created_at
		) VALUES ('ctx_01', 'usr_01', 'grant', 100, 'polar-grant-01', now(), now())`); err != nil {
		t.Fatalf("insert Credit Transaction: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		UPDATE credit_transactions SET credits = 200 WHERE id = 'ctx_01'`), "23514", "rewrite Credit Transaction")
	assertPostgreSQLCode(t, execError(ctx, database, `
		DELETE FROM credit_transactions WHERE id = 'ctx_01'`), "23514", "delete Credit Transaction")
}

func TestMigrationsKeepComputeUsageIdentityImmutable(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	if err := insertEnvironment(ctx, database, "env_01", "usr_01", "prv_01", "workspace"); err != nil {
		t.Fatalf("insert Environment: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO compute_usage_intervals (
			id, user_id, environment_id, runtime_id, region, runtime_preset, started_at
		) VALUES ('cui_01', 'usr_01', 'env_01', 'run_01', 'us-east-1', 'standard', now())`); err != nil {
		t.Fatalf("insert Compute Usage Interval: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		UPDATE compute_usage_intervals SET runtime_id = 'run_replacement' WHERE id = 'cui_01'`), "23514", "rewrite Compute Usage Interval identity")
	assertPostgreSQLCode(t, execError(ctx, database, `
		DELETE FROM compute_usage_intervals WHERE id = 'cui_01'`), "23514", "delete Compute Usage Interval history")
}
