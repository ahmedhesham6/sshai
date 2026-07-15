package db_test

import (
	"context"
	"testing"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
)

func TestMigrationsEnforceImmutableUploadIntents(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertUser(t, ctx, database, "user-1", "workos-1")
	if _, err := database.ExecContext(ctx, `
		INSERT INTO upload_intents (id, owner_user_id, kind, digest, size_bytes, object_key, created_at, expires_at)
		VALUES ('upload-1', 'user-1', 'profile_artifact', 'sha256:' || repeat('a', 64), 42, 'uploads/profile_artifact/key', now(), now() + interval '10 minutes')`); err != nil {
		t.Fatalf("insert Upload Intent: %v", err)
	}
	for name, statement := range map[string]string{
		"unknown kind":   `INSERT INTO upload_intents (id, owner_user_id, kind, digest, size_bytes, object_key, created_at, expires_at) VALUES ('bad-kind', 'user-1', 'archive', 'sha256:' || repeat('a', 64), 1, 'key', now(), now() + interval '1 minute')`,
		"negative size":  `INSERT INTO upload_intents (id, owner_user_id, kind, digest, size_bytes, object_key, created_at, expires_at) VALUES ('bad-size', 'user-1', 'profile_artifact', 'sha256:' || repeat('a', 64), -1, 'key', now(), now() + interval '1 minute')`,
		"invalid expiry": `INSERT INTO upload_intents (id, owner_user_id, kind, digest, size_bytes, object_key, created_at, expires_at) VALUES ('bad-time', 'user-1', 'profile_artifact', 'sha256:' || repeat('a', 64), 1, 'key', now(), now())`,
		"mutate intent":  `UPDATE upload_intents SET expires_at = expires_at + interval '1 minute' WHERE id = 'upload-1'`,
		"delete intent":  `DELETE FROM upload_intents WHERE id = 'upload-1'`,
	} {
		t.Run(name, func(t *testing.T) { assertPostgreSQLCode(t, execError(ctx, database, statement), "23514", name) })
	}
}
