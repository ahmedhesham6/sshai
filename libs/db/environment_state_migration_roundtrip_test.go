package db

import (
	"context"
	"io/fs"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/pressly/goose/v3"
)

func TestEnvironmentStateMigrationDownUpRoundTripRemovesOwnedInventory(t *testing.T) {
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
		`INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version) VALUES ('environment-1', 'user-1', 'dev', 'dev', 'creating', 'unknown', 'us-east-1', 'us-east-1a', 'standard', 'profile-version-1', 1)`,
		`INSERT INTO auto_stop_policies (id, environment_id, mode, grace_period_seconds) VALUES ('policy-1', 'environment-1', 'manual', 0)`,
		`INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, input) VALUES ('operation-1', 'environment-1', 'environment.create', 'queued', 'user-1', 'request-1', '{}')`,
		`INSERT INTO workflow_outbox (operation_id, kind, created_at) VALUES ('operation-1', 'environment.create', now())`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed Environment State migration: %v", err)
		}
	}
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin State Component seed: %v", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO provider_resources (
			id, environment_id, operation_id, provider, region, resource_type, provider_id, metadata
		) VALUES ('resource-1', 'environment-1', 'operation-1', 'aws', 'us-east-1', 'data_volume', 'volume-1', '{}')`); err != nil {
		_ = transaction.Rollback()
		t.Fatalf("seed Provider Resource: %v", err)
	}
	componentStatements := []string{
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('workspace-1', 'environment-1', 'workspace', 'durable', '/workspace', 'resource-1', 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('home-1', 'environment-1', 'home', 'durable', '/home/dev', 'resource-1', 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('services-1', 'environment-1', 'services', 'durable', '/var/lib/docker', 'resource-1', 'data_volume', 'unknown')`,
		`INSERT INTO state_components (id, environment_id, kind, durability, mount_path, backend_resource_id, backend_resource_type, health) VALUES ('cache-1', 'environment-1', 'cache', 'disposable', '/var/cache/devm', 'resource-1', 'data_volume', 'unknown')`,
	}
	for _, statement := range componentStatements {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			_ = transaction.Rollback()
			t.Fatalf("seed State Component: %v", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit State Components: %v", err)
	}

	if _, err := provider.DownTo(ctx, 12); err != nil {
		t.Fatalf("migrate down to 12: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up after rollback: %v", err)
	}
	var providerCount, componentCount int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM provider_resources`).Scan(&providerCount); err != nil {
		t.Fatalf("count Provider Resources after round trip: %v", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM state_components`).Scan(&componentCount); err != nil {
		t.Fatalf("count State Components after round trip: %v", err)
	}
	if providerCount != 0 || componentCount != 0 {
		t.Fatalf("round trip retained Provider Resources/State Components = %d/%d", providerCount, componentCount)
	}
}
