package db_test

import (
	"context"
	"testing"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
)

func TestMigrationsEnforceImmutableProfilePublication(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	insertUser(t, ctx, database, "usr_01", "workos_01")
	insertProfile(t, ctx, database, "pro_01", "usr_01", "personal")

	if _, err := database.ExecContext(ctx, `
		INSERT INTO profile_versions (id, profile_id, version, digest)
		VALUES ('prv_01', 'pro_01', 1, 'sha256:' || repeat('a', 64))`); err != nil {
		t.Fatalf("insert first Profile Version: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO profile_artifacts (
			id, profile_version_id, kind, source_locator, source_digest, content_digest,
			size_bytes, mode, sensitivity, trust, contains_executable
		) VALUES (
			'art_01', 'prv_01', 'agent_instruction', 'AGENTS.md#$',
			'sha256:' || repeat('b', 64), 'sha256:' || repeat('c', 64), 42, 416,
			'private', 'user_authored', false
		)`); err != nil {
		t.Fatalf("insert Profile Artifact: %v", err)
	}

	for name, statement := range map[string]string{
		"version digest":        `INSERT INTO profile_versions (id, profile_id, parent_version_id, version, digest) VALUES ('prv_bad_digest', 'pro_01', 'prv_01', 2, 'invalid')`,
		"skipped parent":        `INSERT INTO profile_versions (id, profile_id, parent_version_id, version, digest) VALUES ('prv_skip', 'pro_01', 'prv_01', 3, 'sha256:' || repeat('d', 64))`,
		"artifact digest":       `INSERT INTO profile_artifacts (id, profile_version_id, kind, source_locator, source_digest, content_digest, size_bytes, mode, sensitivity, trust, contains_executable) VALUES ('art_bad_digest', 'prv_01', 'agent_instruction', 'bad-digest', 'invalid', 'sha256:' || repeat('c', 64), 1, 420, 'private', 'user_authored', false)`,
		"negative size":         `INSERT INTO profile_artifacts (id, profile_version_id, kind, source_locator, source_digest, content_digest, size_bytes, mode, sensitivity, trust, contains_executable) VALUES ('art_bad_size', 'prv_01', 'agent_instruction', 'bad-size', 'sha256:' || repeat('b', 64), 'sha256:' || repeat('c', 64), -1, 420, 'private', 'user_authored', false)`,
		"invalid mode":          `INSERT INTO profile_artifacts (id, profile_version_id, kind, source_locator, source_digest, content_digest, size_bytes, mode, sensitivity, trust, contains_executable) VALUES ('art_bad_mode', 'prv_01', 'agent_instruction', 'bad-mode', 'sha256:' || repeat('b', 64), 'sha256:' || repeat('c', 64), 1, 512, 'private', 'user_authored', false)`,
		"credential artifact":   `INSERT INTO profile_artifacts (id, profile_version_id, kind, source_locator, source_digest, content_digest, size_bytes, mode, sensitivity, trust, contains_executable) VALUES ('art_credential', 'prv_01', 'agent_instruction', 'credential', 'sha256:' || repeat('b', 64), 'sha256:' || repeat('c', 64), 1, 420, 'credential', 'user_authored', false)`,
		"wrong executable flag": `INSERT INTO profile_artifacts (id, profile_version_id, kind, source_locator, source_digest, content_digest, size_bytes, mode, sensitivity, trust, contains_executable) VALUES ('art_exec', 'prv_01', 'agent_skill_executable', 'skill', 'sha256:' || repeat('b', 64), 'sha256:' || repeat('c', 64), 1, 493, 'private', 'user_authored', false)`,
		"unknown trust":         `INSERT INTO profile_artifacts (id, profile_version_id, kind, source_locator, source_digest, content_digest, size_bytes, mode, sensitivity, trust, contains_executable) VALUES ('art_trust', 'prv_01', 'agent_instruction', 'trust', 'sha256:' || repeat('b', 64), 'sha256:' || repeat('c', 64), 1, 420, 'private', 'unknown', false)`,
		"mutate version":        `UPDATE profile_versions SET digest = 'sha256:' || repeat('e', 64) WHERE id = 'prv_01'`,
		"delete version":        `DELETE FROM profile_versions WHERE id = 'prv_01'`,
		"mutate artifact":       `UPDATE profile_artifacts SET source_locator = 'changed' WHERE id = 'art_01'`,
		"delete artifact":       `DELETE FROM profile_artifacts WHERE id = 'art_01'`,
	} {
		t.Run(name, func(t *testing.T) {
			assertPostgreSQLCode(t, execError(ctx, database, statement), "23514", name)
		})
	}
}
