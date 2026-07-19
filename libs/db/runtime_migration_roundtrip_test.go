package db

import (
	"context"
	"io/fs"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/pressly/goose/v3"
)

func TestRuntimeMigrationDownUpRoundTripRemovesRuntimeOwnedState(t *testing.T) {
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
		`INSERT INTO runtimes (id, environment_id, sequence, status, runtime_preset, region, availability_zone, image_version, version) VALUES ('runtime-1', 'environment-1', 1, 'absent', 'standard', 'us-east-1', 'us-east-1a', 'image-1', 1)`,
		`UPDATE environments SET current_runtime_id = 'runtime-1' WHERE id = 'environment-1'`,
		`INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, input) VALUES ('operation-1', 'environment-1', 'runtime.start', 'queued', 'user-1', 'request-1', '{}')`,
		`INSERT INTO runtime_operation_targets (operation_id, environment_id, runtime_id, operation_type) VALUES ('operation-1', 'environment-1', 'runtime-1', 'runtime.start')`,
		`INSERT INTO workflow_outbox (operation_id, kind, created_at) VALUES ('operation-1', 'runtime.start', now())`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed Runtime migration state: %v", err)
		}
	}
	if _, err := provider.DownTo(ctx, 10); err != nil {
		t.Fatalf("migrate down to 10: %v", err)
	}
	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatalf("migrate up after rollback: %v", err)
	}
	var currentRuntimeID *string
	if err := database.QueryRowContext(ctx, `SELECT current_runtime_id FROM environments WHERE id = 'environment-1'`).Scan(&currentRuntimeID); err != nil {
		t.Fatalf("read Environment after round trip: %v", err)
	}
	var operationCount int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE id = 'operation-1'`).Scan(&operationCount); err != nil {
		t.Fatalf("count Runtime Operations after round trip: %v", err)
	}
	if currentRuntimeID != nil || operationCount != 0 {
		t.Fatalf("round trip retained current Runtime/Operation = %v/%d", currentRuntimeID, operationCount)
	}
}

func TestRuntimeProviderResourceMigrationDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	database, _ := testfixtures.OpenPostgres(t, ctx)
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate through Runtime Provider Resource inventory: %v", err)
	}
	if _, err := provider.DownTo(ctx, 19); err != nil {
		t.Fatalf("migrate Runtime Provider Resource inventory down: %v", err)
	}
	assertMigrationColumnCount(t, ctx, database, "provider_resources", "runtime_id", 0)
	if _, err := provider.UpTo(ctx, 20); err != nil {
		t.Fatalf("migrate Runtime Provider Resource inventory up again: %v", err)
	}
	assertMigrationColumnCount(t, ctx, database, "provider_resources", "runtime_id", 1)
	assertMigrationColumnCount(t, ctx, database, "runtimes", "provider_resource_id", 1)
}
