package db

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
)

func TestEnvironmentStateMigrationSerializesAgainstWorkflowStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database, provider := environmentStateMigrationProvider(t, ctx)
	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatalf("migrate to version 12: %v", err)
	}
	seedLegacyEnvironment(t, ctx, database, "environment-1", "creating", "dev")
	seedLegacyCreationProvenance(t, ctx, database, "environment-1", "queued-unstarted")

	ddlBarrier, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin DDL barrier: %v", err)
	}
	defer func() { _ = ddlBarrier.Rollback() }()
	if _, err := ddlBarrier.ExecContext(ctx, `LOCK TABLE environments IN ACCESS SHARE MODE`); err != nil {
		t.Fatalf("lock Environment DDL barrier: %v", err)
	}
	migrationResult := make(chan error, 1)
	go func() {
		_, err := provider.UpTo(ctx, 13)
		migrationResult <- err
	}()
	if !awaitDatabaseCondition(ctx, database, `
		SELECT count(DISTINCT relation::regclass::text) = 2
		FROM pg_locks
		WHERE granted
		  AND mode = 'ShareRowExclusiveLock'
		  AND relation IN ('operations'::regclass, 'workflow_outbox'::regclass)`) {
		_ = ddlBarrier.Rollback()
		t.Fatalf("migration did not lock creation provenance before DDL: %v", <-migrationResult)
	}

	startConnection, err := database.Conn(ctx)
	if err != nil {
		t.Fatalf("open workflow-start connection: %v", err)
	}
	defer startConnection.Close()
	var startPID int
	if err := startConnection.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&startPID); err != nil {
		t.Fatalf("read workflow-start backend PID: %v", err)
	}
	workflowStart, err := startConnection.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin workflow start: %v", err)
	}
	defer func() { _ = workflowStart.Rollback() }()
	workflowResult := make(chan error, 1)
	go func() {
		if _, err := workflowStart.ExecContext(ctx, `
			UPDATE operations SET restate_invocation_id = 'invocation-1'
			WHERE id = 'operation-environment-1'`); err != nil {
			workflowResult <- err
			return
		}
		if _, err := workflowStart.ExecContext(ctx, `
			UPDATE workflow_outbox
			SET started_at = created_at + interval '1 second', restate_invocation_id = 'invocation-1'
			WHERE operation_id = 'operation-environment-1'`); err != nil {
			workflowResult <- err
			return
		}
		workflowResult <- workflowStart.Commit()
	}()
	if !awaitDatabaseCondition(ctx, database, `
		SELECT wait_event_type = 'Lock' FROM pg_stat_activity WHERE pid = $1`, startPID) {
		t.Fatal("workflow start was not blocked by migration provenance lock")
	}
	if err := ddlBarrier.Commit(); err != nil {
		t.Fatalf("release DDL barrier: %v", err)
	}
	if err := <-migrationResult; err != nil {
		t.Fatalf("complete serialized migration: %v", err)
	}
	if err := <-workflowResult; err != nil {
		t.Fatalf("complete workflow start after migration: %v", err)
	}
	var version int64
	if err := database.QueryRowContext(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil {
		t.Fatalf("read installed migration version: %v", err)
	}
	if version != 13 {
		t.Fatalf("workflow start resumed before migration 13 was installed: version %d", version)
	}
}

func TestEnvironmentStateMigrationPreflightsUninventoriedLegacyLifecycle(t *testing.T) {
	for _, lifecycle := range []string{"active", "deleting"} {
		t.Run(lifecycle, func(t *testing.T) {
			ctx := context.Background()
			database, provider := environmentStateMigrationProvider(t, ctx)
			if _, err := provider.UpTo(ctx, 12); err != nil {
				t.Fatalf("migrate to version 12: %v", err)
			}
			seedLegacyEnvironment(t, ctx, database, "environment-1", lifecycle, "dev")
			_, err := provider.UpTo(ctx, 13)
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
				t.Fatalf("migrate uninventoried %s Environment error = %v", lifecycle, err)
			}
		})
	}
}

func TestEnvironmentStateMigrationAllowsLegacyLifecycleWithoutCurrentState(t *testing.T) {
	ctx := context.Background()
	database, provider := environmentStateMigrationProvider(t, ctx)
	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatalf("migrate to version 12: %v", err)
	}
	seedLegacyEnvironment(t, ctx, database, "environment-creating", "creating", "creating")
	seedLegacyCreationProvenance(t, ctx, database, "environment-creating", "queued-unstarted")
	seedLegacyEnvironment(t, ctx, database, "environment-deleted", "deleted", "deleted")
	if _, err := provider.UpTo(ctx, 13); err != nil {
		t.Fatalf("migrate allowed legacy lifecycles: %v", err)
	}
	var components, resources int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM state_components`).Scan(&components); err != nil {
		t.Fatalf("count State Components: %v", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM provider_resources`).Scan(&resources); err != nil {
		t.Fatalf("count Provider Resources: %v", err)
	}
	if components != 0 || resources != 0 {
		t.Fatalf("legacy empty State inventory = %d/%d", components, resources)
	}
}

func TestEnvironmentStateMigrationRejectsAmbiguousLegacyCreation(t *testing.T) {
	for _, provenance := range []string{"missing", "started", "running", "terminal"} {
		t.Run(provenance, func(t *testing.T) {
			ctx := context.Background()
			database, provider := environmentStateMigrationProvider(t, ctx)
			if _, err := provider.UpTo(ctx, 12); err != nil {
				t.Fatalf("migrate to version 12: %v", err)
			}
			seedLegacyEnvironment(t, ctx, database, "environment-1", "creating", "dev")
			if provenance != "missing" {
				seedLegacyCreationProvenance(t, ctx, database, "environment-1", provenance)
			}
			_, err := provider.UpTo(ctx, 13)
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
				t.Fatalf("migrate %s legacy creation error = %v", provenance, err)
			}
		})
	}
}

func environmentStateMigrationProvider(t *testing.T, ctx context.Context) (*sql.DB, *goose.Provider) {
	t.Helper()
	database, _ := testfixtures.OpenPostgres(t, ctx)
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("open migrations: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrations)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	return database, provider
}

func awaitDatabaseCondition(ctx context.Context, database *sql.DB, query string, arguments ...any) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var satisfied bool
		if err := database.QueryRowContext(ctx, query, arguments...).Scan(&satisfied); err == nil && satisfied {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func seedLegacyEnvironment(t *testing.T, ctx context.Context, database *sql.DB, environmentID, lifecycle, slug string) {
	t.Helper()
	userID, profileID, versionID := "user-"+environmentID, "profile-"+environmentID, "version-"+environmentID
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users (id, workos_user_id, default_region) VALUES ($1, $2, 'us-east-1')`, []any{userID, "workos-" + environmentID}},
		{`INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ($1, $2, 'Default', $3)`, []any{profileID, userID, slug}},
		{`INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ($1, $2, 1, 'sha256:' || repeat('c', 64))`, []any{versionID, profileID}},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed legacy prerequisite: %v", err)
		}
	}
	if lifecycle == "deleted" {
		_, err := database.ExecContext(ctx, `
			INSERT INTO environments (
				id, owner_user_id, name, slug, lifecycle, health, region, availability_zone,
				runtime_preset, pinned_profile_version_id, updated_at, deleted_at, version
			) VALUES ($1, $2, 'dev', $3, 'deleted', 'unknown', 'us-east-1', 'us-east-1a',
				'standard', $4, now(), now(), 1)`, environmentID, userID, slug, versionID)
		if err != nil {
			t.Fatalf("seed deleted legacy Environment: %v", err)
		}
		return
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO environments (
			id, owner_user_id, name, slug, lifecycle, health, region, availability_zone,
			runtime_preset, pinned_profile_version_id, version
		) VALUES ($1, $2, 'dev', $3, $4, 'unknown', 'us-east-1', 'us-east-1a',
			'standard', $5, 1)`, environmentID, userID, slug, lifecycle, versionID); err != nil {
		t.Fatalf("seed %s legacy Environment: %v", lifecycle, err)
	}
}

func seedLegacyCreationProvenance(t *testing.T, ctx context.Context, database *sql.DB, environmentID, provenance string) {
	t.Helper()
	status, invocationID, completedAt := "queued", any(nil), any(nil)
	if provenance == "started" {
		invocationID = "invocation-1"
	}
	if provenance == "running" {
		status, invocationID = "running", "invocation-1"
	}
	if provenance == "terminal" {
		status, invocationID, completedAt = "failed", "invocation-1", "2026-07-13T18:00:00Z"
	}
	userID := "user-" + environmentID
	if _, err := database.ExecContext(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key,
			restate_invocation_id, input, created_at, completed_at
		) VALUES ($1, $2, 'environment.create', $3, $4, $5, $6, '{}', '2026-07-13T17:58:00Z', $7)`,
		"operation-"+environmentID, environmentID, status, userID, "key-"+environmentID, invocationID, completedAt); err != nil {
		t.Fatalf("seed legacy creation Operation: %v", err)
	}
	var startedAt any
	if provenance != "queued-unstarted" {
		startedAt = "2026-07-13T17:59:00Z"
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO workflow_outbox (operation_id, kind, created_at, started_at, restate_invocation_id)
		VALUES ($1, 'environment.create', '2026-07-13T17:58:00Z', $2, $3)`,
		"operation-"+environmentID, startedAt, invocationID); err != nil {
		t.Fatalf("seed legacy creation outbox: %v", err)
	}
}
