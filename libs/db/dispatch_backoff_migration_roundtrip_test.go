package db

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/pressly/goose/v3"
)

func TestDispatchBackoffMigrationDownUpRoundTrip(t *testing.T) {
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
		t.Fatalf("migrate through dispatch backoff: %v", err)
	}
	for _, column := range []struct {
		table string
		name  string
	}{
		{table: "workflow_outbox", name: "dispatch_attempts"},
		{table: "workflow_outbox", name: "next_attempt_at"},
		{table: "auto_stop_policies", name: "refresh_attempts"},
		{table: "auto_stop_policies", name: "refresh_next_attempt_at"},
	} {
		assertMigrationColumnCount(t, ctx, database, column.table, column.name, 1)
	}
	if _, err := provider.DownTo(ctx, 18); err != nil {
		t.Fatalf("migrate dispatch backoff down: %v", err)
	}
	assertMigrationColumnCount(t, ctx, database, "workflow_outbox", "dispatch_attempts", 0)
	assertMigrationColumnCount(t, ctx, database, "auto_stop_policies", "refresh_attempts", 0)
	if _, err := provider.UpTo(ctx, 19); err != nil {
		t.Fatalf("migrate dispatch backoff up again: %v", err)
	}
	assertMigrationColumnCount(t, ctx, database, "workflow_outbox", "dispatch_attempts", 1)
	assertMigrationColumnCount(t, ctx, database, "auto_stop_policies", "refresh_attempts", 1)
}

func assertMigrationColumnCount(t *testing.T, ctx context.Context, database *sql.DB, table, column string, want int) {
	t.Helper()
	var count int
	if err := database.QueryRowContext(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, table, column).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("column %s.%s count = %d, want %d", table, column, count, want)
	}
}
