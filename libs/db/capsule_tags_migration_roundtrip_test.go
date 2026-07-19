package db

import (
	"context"
	"io/fs"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/pressly/goose/v3"
)

func TestCapsuleTagMigrationDownUpRoundTrip(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 24); err != nil {
		t.Fatalf("migrate through Capsule tag index: %v", err)
	}
	assertMigrationColumnCount(t, ctx, database, "capsule_tags", "digest", 1)
	if _, err := provider.DownTo(ctx, 23); err != nil {
		t.Fatalf("migrate Capsule tag index down: %v", err)
	}
	assertMigrationColumnCount(t, ctx, database, "capsule_tags", "digest", 0)
	if _, err := provider.UpTo(ctx, 24); err != nil {
		t.Fatalf("migrate Capsule tag index up again: %v", err)
	}
	assertMigrationColumnCount(t, ctx, database, "capsule_tags", "digest", 1)
}
