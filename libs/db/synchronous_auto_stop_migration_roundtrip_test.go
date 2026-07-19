package db

import (
	"context"
	"io/fs"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/pressly/goose/v3"
)

func TestSynchronousAutoStopOperationMigrationDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	database, _ := testfixtures.OpenPostgres(t, ctx)
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("open migrations: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrations)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	statements := []string{
		`INSERT INTO users (id, workos_user_id, default_region) VALUES ('user-1', 'workos-1', 'us-east-1')`,
		`INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-1', 'user-1', 'Default', 'default')`,
		`INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('profile-version-1', 'profile-1', 1, 'sha256:' || repeat('c', 64))`,
		`INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version) VALUES ('environment-1', 'user-1', 'dev', 'dev', 'active', 'healthy', 'us-east-1', 'us-east-1a', 'standard', 'profile-version-1', 1)`,
		`INSERT INTO auto_stop_policies (id, environment_id, mode, grace_period_seconds) VALUES ('policy-1', 'environment-1', 'manual', 0)`,
		`INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, input, created_at, completed_at) VALUES ('operation-policy-1', 'environment-1', 'environment.update_auto_stop', 'succeeded', 'user-1', 'request-policy-1', '{"gracePeriodSeconds":0,"mode":"manual"}', now(), now())`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed synchronous Auto-stop Operation migration: %v", err)
		}
	}

	if _, err := provider.DownTo(ctx, 16); err != nil {
		t.Fatalf("migrate down to 16: %v", err)
	}
	var operationCount int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE id = 'operation-policy-1'`).Scan(&operationCount); err != nil {
		t.Fatalf("count synchronous Operation after rollback: %v", err)
	}
	if operationCount != 0 {
		t.Fatalf("synchronous Operations after rollback = %d, want 0", operationCount)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key,
			input, created_at, completed_at
		) VALUES (
			'operation-policy-rejected', 'environment-1', 'environment.update_auto_stop', 'succeeded',
			'user-1', 'request-policy-rejected', '{"gracePeriodSeconds":0,"mode":"manual"}', now(), now()
		)`); err == nil {
		t.Fatal("migration 16 accepted succeeded Operation without Restate invocation")
	}

	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatalf("migrate up to 17: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key,
			input, created_at, completed_at
		) VALUES (
			'operation-policy-2', 'environment-1', 'environment.update_auto_stop', 'succeeded',
			'user-1', 'request-policy-2', '{"gracePeriodSeconds":0,"mode":"manual"}', now(), now()
		)`); err != nil {
		t.Fatalf("migration 17 rejected synchronous Auto-stop Operation after round trip: %v", err)
	}
}
