package db_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationsEnforceUniqueWorkOSUserID(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("repeat database migration: %v", err)
	}

	const insertUser = `
		INSERT INTO users (id, workos_user_id, default_region)
		VALUES ($1, $2, 'us-east-1')`
	if _, err := database.ExecContext(ctx, insertUser, "usr_01", "workos_01"); err != nil {
		t.Fatalf("insert first User: %v", err)
	}
	_, err := database.ExecContext(ctx, insertUser, "usr_02", "workos_01")
	assertPostgreSQLCode(t, err, "23505", "duplicate WorkOS user ID")
}

func TestMigrationsEnforceActiveEnvironmentSlugPerOwner(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertEnvironmentPrerequisites(t, ctx, database, "usr_02", "workos_02", "pro_02", "prv_02")
	if err := insertEnvironment(ctx, database, "env_01", "usr_01", "prv_01", "api-dev"); err != nil {
		t.Fatalf("insert first Environment: %v", err)
	}

	err := insertEnvironment(ctx, database, "env_02", "usr_01", "prv_01", "api-dev")
	assertPostgreSQLCode(t, err, "23505", "duplicate active Environment slug")
	if err := insertEnvironment(ctx, database, "env_03", "usr_02", "prv_02", "api-dev"); err != nil {
		t.Fatalf("insert same Environment slug for another owner: %v", err)
	}

	deletedAt := time.Now().UTC()
	if _, err := database.ExecContext(ctx, `
		UPDATE environments
		SET lifecycle = 'deleted', updated_at = $2, deleted_at = $2
		WHERE id = $1`, "env_01", deletedAt); err != nil {
		t.Fatalf("delete first Environment: %v", err)
	}
	if err := insertEnvironment(ctx, database, "env_04", "usr_01", "prv_01", "api-dev"); err != nil {
		t.Fatalf("reuse deleted Environment slug: %v", err)
	}
}

func TestMigrationsEnforceActiveProfileSlugPerOwner(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	insertUser(t, ctx, database, "usr_01", "workos_01")
	insertUser(t, ctx, database, "usr_02", "workos_02")
	insertProfile(t, ctx, database, "pro_01", "usr_01", "personal")

	err := insertProfileError(ctx, database, "pro_02", "usr_01", "personal")
	assertPostgreSQLCode(t, err, "23505", "duplicate active Profile slug")
	insertProfile(t, ctx, database, "pro_03", "usr_02", "personal")

	if _, err := database.ExecContext(ctx, `UPDATE profiles SET archived_at = now() WHERE id = 'pro_01'`); err != nil {
		t.Fatalf("archive first Profile: %v", err)
	}
	insertProfile(t, ctx, database, "pro_04", "usr_01", "personal")
}

func TestMigrationsEnforceProfileLocalLinearVersionHistory(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	insertUser(t, ctx, database, "usr_01", "workos_01")
	insertProfile(t, ctx, database, "pro_01", "usr_01", "first")
	insertProfile(t, ctx, database, "pro_02", "usr_01", "second")
	if err := insertProfileVersionError(ctx, database, "prv_01", "pro_01", nil, 1); err != nil {
		t.Fatalf("insert first Profile Version: %v", err)
	}

	parentID := "prv_01"
	err := insertProfileVersionError(ctx, database, "prv_02", "pro_02", &parentID, 1)
	assertPostgreSQLCode(t, err, "23503", "cross-Profile parent")

	selfID := "prv_02"
	err = insertProfileVersionError(ctx, database, selfID, "pro_01", &selfID, 2)
	assertPostgreSQLCode(t, err, "23514", "self parent")

	if err := insertProfileVersionError(ctx, database, "prv_02", "pro_01", &parentID, 2); err != nil {
		t.Fatalf("insert child Profile Version: %v", err)
	}
	err = insertProfileVersionError(ctx, database, "prv_03", "pro_01", &parentID, 3)
	assertPostgreSQLCode(t, err, "23514", "skipped parent")
}

func TestMigrationsEnforceSSHKeyFingerprintPerUser(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	insertUser(t, ctx, database, "usr_01", "workos_01")
	insertUser(t, ctx, database, "usr_02", "workos_02")
	insertSSHKey := func(id, ownerID string) error {
		_, err := database.ExecContext(ctx, `
			INSERT INTO ssh_keys (id, owner_user_id, label, algorithm, fingerprint, public_key)
			VALUES ($1, $2, 'laptop', 'ssh-ed25519', 'SHA256:shared', 'ssh-ed25519 AAAA')`, id, ownerID)
		return err
	}
	if err := insertSSHKey("key_01", "usr_01"); err != nil {
		t.Fatalf("insert first SSH Key: %v", err)
	}
	assertPostgreSQLCode(t, insertSSHKey("key_02", "usr_01"), "23505", "duplicate SSH Key fingerprint")
	if _, err := database.ExecContext(ctx, `UPDATE ssh_keys SET revoked_at = now() WHERE id = 'key_01'`); err != nil {
		t.Fatalf("revoke first SSH Key: %v", err)
	}
	assertPostgreSQLCode(t, insertSSHKey("key_03", "usr_01"), "23505", "revoked SSH Key fingerprint reuse")
	if err := insertSSHKey("key_04", "usr_02"); err != nil {
		t.Fatalf("insert same SSH Key fingerprint for another User: %v", err)
	}
}

func TestMigrationsEnforceImmutableSingleUseProjectSeed(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertEnvironmentPrerequisites(t, ctx, database, "usr_02", "workos_02", "pro_02", "prv_02")
	if err := insertEnvironment(ctx, database, "env_01", "usr_01", "prv_01", "first"); err != nil {
		t.Fatalf("insert first Environment: %v", err)
	}
	if err := insertEnvironment(ctx, database, "env_02", "usr_01", "prv_01", "second"); err != nil {
		t.Fatalf("insert second Environment: %v", err)
	}
	if err := insertEnvironment(ctx, database, "env_03", "usr_02", "prv_02", "third"); err != nil {
		t.Fatalf("insert foreign Environment: %v", err)
	}

	insertProjectSeed := func(id, ownerID string) {
		t.Helper()
		if _, err := database.ExecContext(ctx, `
			INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, manifest_digest)
			VALUES ($1, $2, 'https://github.com/example/project.git', 'abc123', $3, 'sha256:' || repeat('b', 64))`, id, ownerID, digest(id[len(id)-1])); err != nil {
			t.Fatalf("insert Project Seed %s: %v", id, err)
		}
	}
	insertProjectSeed("sed_01", "usr_01")
	for _, invalid := range []struct{ name, statement string }{
		{name: "overall", statement: `
		INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, manifest_digest)
		VALUES ('sed_invalid_overall', 'usr_01', 'https://github.com/example/project.git', 'abc123', 'invalid', 'sha256:' || repeat('b', 64))`},
		{name: "manifest", statement: `
		INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, manifest_digest)
		VALUES ('sed_invalid_manifest', 'usr_01', 'https://github.com/example/project.git', 'abc123', 'sha256:' || repeat('c', 64), 'invalid')`},
		{name: "Git bundle", statement: `
		INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, git_bundle_digest, manifest_digest)
		VALUES ('sed_invalid_git', 'usr_01', 'https://github.com/example/project.git', 'abc123', 'sha256:' || repeat('c', 64), 'invalid', 'sha256:' || repeat('b', 64))`},
		{name: "tracked patch", statement: `
		INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, tracked_patch_digest, manifest_digest)
		VALUES ('sed_invalid_patch', 'usr_01', 'https://github.com/example/project.git', 'abc123', 'sha256:' || repeat('c', 64), 'invalid', 'sha256:' || repeat('b', 64))`},
		{name: "untracked bundle", statement: `
		INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, untracked_bundle_digest, manifest_digest)
		VALUES ('sed_invalid_untracked', 'usr_01', 'https://github.com/example/project.git', 'abc123', 'sha256:' || repeat('c', 64), 'invalid', 'sha256:' || repeat('b', 64))`},
	} {
		t.Run("invalid "+invalid.name+" digest", func(t *testing.T) {
			assertPostgreSQLCode(t, execError(ctx, database, invalid.statement), "23514", "invalid "+invalid.name+" Project Seed digest")
		})
	}
	var gitBundle, trackedPatch, untrackedBundle sql.NullString
	if err := database.QueryRowContext(ctx, `
		SELECT git_bundle_digest, tracked_patch_digest, untracked_bundle_digest
		FROM project_seeds WHERE id = 'sed_01'`).Scan(&gitBundle, &trackedPatch, &untrackedBundle); err != nil {
		t.Fatalf("read optional Project Seed digests: %v", err)
	}
	if gitBundle.Valid || trackedPatch.Valid || untrackedBundle.Valid {
		t.Fatal("absent optional Project Seed digests were not stored as NULL")
	}
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE project_seeds SET base_revision = 'changed' WHERE id = 'sed_01'`), "23514", "mutate Project Seed content")
	if _, err := database.ExecContext(ctx, `UPDATE project_seeds SET environment_id = 'env_01' WHERE id = 'sed_01'`); err != nil {
		t.Fatalf("assign Project Seed: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE project_seeds SET environment_id = NULL WHERE id = 'sed_01'`), "23514", "unassign Project Seed")
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE project_seeds SET environment_id = 'env_02' WHERE id = 'sed_01'`), "23514", "reassign Project Seed")

	insertProjectSeed("sed_02", "usr_02")
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE project_seeds SET environment_id = 'env_02' WHERE id = 'sed_02'`), "23503", "assign Project Seed across owners")
	insertProjectSeed("sed_03", "usr_01")
	assertPostgreSQLCode(t, execError(ctx, database, `UPDATE project_seeds SET environment_id = 'env_01' WHERE id = 'sed_03'`), "23505", "assign second Project Seed")
}

func TestMigrationsSerializeActiveEnvironmentOperations(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	insertEnvironmentPrerequisites(t, ctx, database, "usr_01", "workos_01", "pro_01", "prv_01")
	insertEnvironmentPrerequisites(t, ctx, database, "usr_02", "workos_02", "pro_02", "prv_02")
	if err := insertEnvironment(ctx, database, "env_01", "usr_01", "prv_01", "first"); err != nil {
		t.Fatalf("insert first Environment: %v", err)
	}
	if err := insertEnvironment(ctx, database, "env_02", "usr_01", "prv_01", "second"); err != nil {
		t.Fatalf("insert second Environment: %v", err)
	}
	if err := insertEnvironment(ctx, database, "env_03", "usr_02", "prv_02", "third"); err != nil {
		t.Fatalf("insert foreign Environment: %v", err)
	}

	insertOperation := func(id, environmentID, userID, idempotencyKey, status string) error {
		var completedAt any
		if status != "queued" && status != "running" {
			completedAt = time.Now().UTC()
		}
		_, err := database.ExecContext(ctx, `
			INSERT INTO operations (
				id, environment_id, type, status, requested_by_user_id,
				idempotency_key, restate_invocation_id, input, completed_at
			) VALUES ($1, $2, 'environment.create', $5, $3, $4, $1, '{}'::jsonb, $6)`,
			id, environmentID, userID, idempotencyKey, status, completedAt)
		return err
	}
	if err := insertOperation("op_01", "env_01", "usr_01", "idem_00000000001", "running"); err != nil {
		t.Fatalf("insert first Operation: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO operation_steps (id, operation_id, step_key, status, summary)
		VALUES ('stp_01', 'op_01', 'reserve', 'pending', 'Reserve Environment')`); err != nil {
		t.Fatalf("insert first Operation Step: %v", err)
	}
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO operation_steps (id, operation_id, step_key, status, summary)
		VALUES ('stp_02', 'op_01', 'reserve', 'pending', 'Reserve again')`), "23505", "duplicate Operation Step")
	now := time.Now().UTC()
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO operation_steps (id, operation_id, step_key, status, summary, started_at)
		VALUES ('stp_03', 'op_01', 'invalid_pending', 'pending', 'Invalid pending', now())`), "23514", "pending Operation Step with start")
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO operation_steps (id, operation_id, step_key, status, summary)
		VALUES ('stp_04', 'op_01', 'invalid_running', 'running', 'Invalid running')`), "23514", "running Operation Step without start")
	assertPostgreSQLCode(t, execError(ctx, database, `
		INSERT INTO operation_steps (id, operation_id, step_key, status, summary, started_at)
		VALUES ('stp_05', 'op_01', 'invalid_terminal', 'succeeded', 'Invalid terminal', now())`), "23514", "terminal Operation Step without completion")
	if _, err := database.ExecContext(ctx, `
		UPDATE operation_steps SET status = 'running', started_at = $2 WHERE id = $1`, "stp_01", now); err != nil {
		t.Fatalf("start Operation Step: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE operation_steps SET status = 'succeeded', completed_at = $2 WHERE id = $1`, "stp_01", now.Add(time.Second)); err != nil {
		t.Fatalf("complete Operation Step: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO operation_steps (id, operation_id, step_key, status, summary, completed_at)
		VALUES ('stp_06', 'op_01', 'not_needed', 'skipped', 'Not needed', $1)`, now); err != nil {
		t.Fatalf("skip Operation Step: %v", err)
	}
	assertPostgreSQLCode(t, insertOperation("op_02", "env_01", "usr_01", "idem_00000000002", "queued"), "23505", "second active Operation")
	assertPostgreSQLCode(t, insertOperation("op_03", "env_02", "usr_01", "idem_00000000001", "queued"), "23505", "duplicate idempotency key")
	assertPostgreSQLCode(t, insertOperation("op_04", "env_02", "usr_02", "idem_00000000004", "queued"), "23503", "cross-owner Operation")

	if _, err := database.ExecContext(ctx, `UPDATE operations SET status = 'succeeded', completed_at = now() WHERE id = 'op_01'`); err != nil {
		t.Fatalf("complete first Operation: %v", err)
	}
	if err := insertOperation("op_05", "env_01", "usr_01", "idem_00000000005", "queued"); err != nil {
		t.Fatalf("insert Operation after completion: %v", err)
	}
	if err := insertOperation("op_06", "env_03", "usr_02", "idem_00000000001", "queued"); err != nil {
		t.Fatalf("reuse idempotency key for another User: %v", err)
	}
}

func execError(ctx context.Context, database *sql.DB, statement string) error {
	_, err := database.ExecContext(ctx, statement)
	return err
}

func insertProfileVersionError(ctx context.Context, database *sql.DB, versionID, profileID string, parentVersionID *string, version int64) error {
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(versionID)))
	_, err := database.ExecContext(ctx, `
		INSERT INTO profile_versions (id, profile_id, parent_version_id, version, digest)
		VALUES ($1, $2, $3, $4, $5)`, versionID, profileID, parentVersionID, version, digest)
	return err
}

func assertPostgreSQLCode(t *testing.T, err error, code, operation string) {
	t.Helper()
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != code {
		t.Fatalf("%s error = %v, want PostgreSQL code %s", operation, err, code)
	}
}

func insertUser(t *testing.T, ctx context.Context, database *sql.DB, userID, workosUserID string) {
	t.Helper()
	if _, err := database.ExecContext(ctx, `
		INSERT INTO users (id, workos_user_id, default_region)
		VALUES ($1, $2, 'us-east-1')`, userID, workosUserID); err != nil {
		t.Fatalf("insert User %s: %v", userID, err)
	}
}

func insertProfile(t *testing.T, ctx context.Context, database *sql.DB, profileID, ownerUserID, slug string) {
	t.Helper()
	if err := insertProfileError(ctx, database, profileID, ownerUserID, slug); err != nil {
		t.Fatalf("insert Profile %s: %v", profileID, err)
	}
}

func insertProfileError(ctx context.Context, database *sql.DB, profileID, ownerUserID, slug string) error {
	_, err := database.ExecContext(ctx, `
		INSERT INTO profiles (id, owner_user_id, name, slug)
		VALUES ($1, $2, $3, $3)`, profileID, ownerUserID, slug)
	return err
}

func insertEnvironmentPrerequisites(t *testing.T, ctx context.Context, database *sql.DB, userID, workosUserID, profileID, profileVersionID string) {
	t.Helper()
	insertUser(t, ctx, database, userID, workosUserID)
	insertProfile(t, ctx, database, profileID, userID, profileID)
	if err := insertProfileVersionError(ctx, database, profileVersionID, profileID, nil, 1); err != nil {
		t.Fatalf("insert Profile Version %s: %v", profileVersionID, err)
	}
}

func insertEnvironment(ctx context.Context, database *sql.DB, environmentID, ownerUserID, profileVersionID, slug string) error {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Environment insert: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO environments (
			id, owner_user_id, name, slug, lifecycle, health, region,
			availability_zone, runtime_preset, pinned_profile_version_id, version
		) VALUES ($1, $2, $3, $4, 'creating', 'unknown', 'us-east-1',
			'us-east-1a', 'standard', $5, 1)`,
		environmentID, ownerUserID, slug, slug, profileVersionID); err != nil {
		return fmt.Errorf("insert Environment: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO auto_stop_policies (id, environment_id, mode, grace_period_seconds)
		VALUES ($1, $2, 'when_disconnected', 600)`, "asp_"+environmentID, environmentID); err != nil {
		return fmt.Errorf("insert Auto-stop Policy: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit Environment insert: %w", err)
	}
	return nil
}
