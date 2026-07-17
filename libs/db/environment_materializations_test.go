package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigrationsCreateEnvironmentPinAndMaterializationSchema(t *testing.T) {
	ctx := context.Background()
	database, _ := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	var capsuleLockID, upgradePolicy string
	if err := database.QueryRowContext(ctx, `
		SELECT capsule_lock_id, upgrade_policy
		FROM environments
		WHERE id = 'environment-1'`,
	).Scan(&capsuleLockID, &upgradePolicy); err == nil {
		t.Fatal("queried pin columns without an Environment row")
	}

	var defaultPolicy string
	if err := database.QueryRowContext(ctx, `
		SELECT column_default
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'environments' AND column_name = 'upgrade_policy'`).Scan(&defaultPolicy); err != nil {
		t.Fatalf("read upgrade policy default: %v", err)
	}
	if defaultPolicy != "'manual'::text" {
		t.Fatalf("upgrade policy default = %q, want manual", defaultPolicy)
	}

	var materializationColumns int
	if err := database.QueryRowContext(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'environment_materializations'
		  AND column_name = ANY($1::text[])`, []string{
		"environment_id", "lock_id", "lock_digest", "capsule_digest", "component_id", "component_digest",
		"adapter_id", "adapter_version", "target_agent_version", "scope", "component_type", "trust_class",
		"effective_cache_key", "last_applied_digest", "observed_digest", "credential_requirement_digest",
		"created_at", "updated_at",
	}).Scan(&materializationColumns); err != nil {
		t.Fatalf("read materialization columns: %v", err)
	}
	if materializationColumns != 18 {
		t.Fatalf("materialization identity columns = %d, want 18", materializationColumns)
	}

	if _, err := database.ExecContext(ctx, `
		INSERT INTO users (id, workos_user_id, default_region)
		VALUES ('user-1', 'workos-1', 'us-east-1')`); err != nil {
		t.Fatalf("insert User: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO profiles (id, owner_user_id, name, slug)
		VALUES ('profile-1', 'user-1', 'Default', 'default')`); err != nil {
		t.Fatalf("insert Profile: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO profile_versions (id, profile_id, version, digest)
		VALUES ('version-1', 'profile-1', 1, 'sha256:' || repeat('a', 64))`); err != nil {
		t.Fatalf("insert Profile Version: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region,
			availability_zone, runtime_preset, pinned_profile_version_id, version)
		VALUES ('environment-1', 'user-1', 'Workspace', 'workspace', 'creating', 'unknown',
			'us-east-1', 'us-east-1a', 'standard', 'version-1', 1)`); err != nil {
		t.Fatalf("insert Environment: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO capsule_locks (id, environment_id, profile_version_id, project_capsule_digest, digest, capsules, resolved_components, created_at)
		VALUES ('lock-1', 'environment-1', 'version-1', 'sha256:' || repeat('b', 64), 'sha256:' || repeat('c', 64), '[]', '{}', now())`); err != nil {
		t.Fatalf("insert Capsule Lock: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE environments SET capsule_lock_id = 'lock-1' WHERE id = 'environment-1'`); err != nil {
		t.Fatalf("pin Environment: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO environment_materializations (
			environment_id, lock_id, lock_digest, capsule_digest, component_id, component_digest,
			adapter_id, adapter_version, target_agent_version, scope, component_type, trust_class,
			effective_cache_key, last_applied_digest, observed_digest, credential_requirement_digest,
			created_at, updated_at
		) VALUES ('environment-1', 'lock-1', 'sha256:' || repeat('c', 64), 'sha256:' || repeat('b', 64),
			'config:editor', 'sha256:' || repeat('d', 64), 'file', 'v1', 'agent-1', 'user', 'config',
			'declarative', 'sha256:' || repeat('e', 64), 'sha256:' || repeat('f', 64), 'sha256:' || repeat('f', 64),
			'sha256:' || repeat('0', 64), now(), now())`); err != nil {
		t.Fatalf("insert Environment Materialization: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO environment_materializations (
			environment_id, lock_id, lock_digest, capsule_digest, component_id, component_digest,
			adapter_id, adapter_version, target_agent_version, scope, component_type, trust_class,
			effective_cache_key, last_applied_digest, observed_digest, credential_requirement_digest,
			created_at, updated_at
		) SELECT environment_id, lock_id, lock_digest, capsule_digest, component_id, component_digest,
			adapter_id, adapter_version, target_agent_version, scope, component_type, trust_class,
			effective_cache_key, last_applied_digest, observed_digest, credential_requirement_digest,
			created_at, updated_at
		FROM environment_materializations WHERE component_id = 'config:editor'`); err == nil {
		t.Fatal("duplicate Environment Materialization component succeeded")
	}
}

func TestMigrationRejectsCrossEnvironmentCapsuleLockPairing(t *testing.T) {
	ctx := context.Background()
	database, connectionString := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	pool, err := pgxpool.New(ctx, connectionString)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	seedEnvironmentMaterializationPrerequisites(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version)
		VALUES ('environment-2', 'user-1', 'Other', 'other', 'creating', 'unknown', 'us-east-1', 'us-east-1a', 'standard', 'version-1', 1)`); err != nil {
		t.Fatalf("insert Environment-2: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO capsule_locks (id, environment_id, profile_version_id, project_capsule_digest, digest, capsules, resolved_components, created_at)
		VALUES ('lock-environment-2', 'environment-2', 'version-1', 'sha256:' || repeat('a', 64),
			'sha256:' || repeat('b', 64), '[]', '{}', now())`); err != nil {
		t.Fatalf("insert Environment-2 Capsule Lock: %v", err)
	}

	_, pinErr := pool.Exec(ctx, `
		UPDATE environments SET capsule_lock_id = 'lock-environment-2' WHERE id = 'environment-1'`)
	_, materializationErr := pool.Exec(ctx, `
		INSERT INTO environment_materializations (
			environment_id, lock_id, lock_digest, capsule_digest, component_id, component_digest,
			adapter_id, adapter_version, target_agent_version, scope, component_type, trust_class,
			effective_cache_key, created_at, updated_at
		) VALUES (
			'environment-1', 'lock-environment-2', 'sha256:' || repeat('b', 64), 'sha256:' || repeat('a', 64),
			'config:editor', 'sha256:' || repeat('c', 64), 'file', 'v1', 'agent-1', 'user', 'config',
			'declarative', 'sha256:' || repeat('d', 64), now(), now()
		)`)
	if pinErr == nil || materializationErr == nil {
		t.Fatalf("cross-Environment pairing errors = pin:%v materialization:%v, want both constrained", pinErr, materializationErr)
	}
}

func TestStorePersistsEnvironmentPinAndRekeysMaterializations(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	seedEnvironmentMaterializationPrerequisites(t, ctx, pool)

	firstLock := persistEnvironmentMaterializationLockFixture(
		t, ctx, store, "lock-1", "environment-1", "version-1", testDigest('b'), now,
		"config:editor", "skill:review",
	)
	secondLock := persistEnvironmentMaterializationLockFixture(
		t, ctx, store, "lock-2", "environment-1", "version-1", testDigest('d'), now.Add(time.Minute),
		"config:editor",
	)
	lockID := firstLock.Snapshot().ID
	secondLockID := secondLock.Snapshot().ID

	policy := domain.UpgradeNotify
	pinned, err := store.UpsertEnvironmentPin(ctx, dbstore.EnvironmentPinInput{
		EnvironmentID: "environment-1", CapsuleLockID: &lockID, UpgradePolicy: policy,
	})
	if err != nil {
		t.Fatalf("UpsertEnvironmentPin(): %v", err)
	}
	if pinned.CapsuleLockID == nil || *pinned.CapsuleLockID != lockID || pinned.UpgradePolicy != policy {
		t.Fatalf("persisted Environment pin = %#v", pinned)
	}
	loadedPin, err := store.GetEnvironmentPin(ctx, "environment-1")
	if err != nil {
		t.Fatalf("GetEnvironmentPin(): %v", err)
	}
	if loadedPin.CapsuleLockID == nil || *loadedPin.CapsuleLockID != lockID || loadedPin.UpgradePolicy != policy {
		t.Fatalf("loaded Environment pin = %#v", loadedPin)
	}

	first := environmentMaterializationFixture("config:editor", lockID, now)
	second := environmentMaterializationFixture("skill:review", lockID, now)
	first.LockDigest = firstLock.Snapshot().Digest
	second.LockDigest = firstLock.Snapshot().Digest
	if err := store.UpsertEnvironmentMaterializations(ctx, []dbstore.EnvironmentMaterialization{first, second}); err != nil {
		t.Fatalf("UpsertEnvironmentMaterializations(): %v", err)
	}
	first.ObservedDigest = testDigest('1')
	if err := store.UpsertEnvironmentMaterializations(ctx, []dbstore.EnvironmentMaterialization{first}); err != nil {
		t.Fatalf("UpsertEnvironmentMaterializations() replay: %v", err)
	}
	loaded, err := store.ListEnvironmentMaterializations(ctx, "environment-1")
	if err != nil {
		t.Fatalf("ListEnvironmentMaterializations(): %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("materialization count = %d, want 2", len(loaded))
	}
	byComponent := make(map[string]dbstore.EnvironmentMaterialization, len(loaded))
	for _, materialization := range loaded {
		byComponent[materialization.ComponentID] = materialization
	}
	if got := byComponent[first.ComponentID]; got.ObservedDigest != first.ObservedDigest || got.SecretVersionIdentifiers[0] != "secret-v1" || got.FilePaths[1] != "settings.json" || !got.Directory {
		t.Fatalf("round-tripped first materialization = %#v", got)
	}

	replacement := environmentMaterializationFixture(first.ComponentID, secondLockID, now.Add(2*time.Minute))
	replacement.LockDigest = secondLock.Snapshot().Digest
	replacement.CapsuleDigest = testDigest('d')
	if err := store.ReplaceEnvironmentMaterializationsForLock(ctx, "environment-1", secondLockID, []dbstore.EnvironmentMaterialization{replacement}); err != nil {
		t.Fatalf("ReplaceEnvironmentMaterializationsForLock(): %v", err)
	}
	loaded, err = store.ListEnvironmentMaterializations(ctx, "environment-1")
	if err != nil {
		t.Fatalf("ListEnvironmentMaterializations() after replace: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ComponentID != first.ComponentID || loaded[0].LockID != secondLockID {
		t.Fatalf("materializations after replace = %#v", loaded)
	}
}

func TestStoreRejectsEnvironmentPinToForeignCapsuleLock(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 12, 30, 0, 0, time.UTC)
	seedEnvironmentMaterializationPrerequisites(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version)
		VALUES ('environment-2', 'user-1', 'Other', 'other', 'creating', 'unknown', 'us-east-1', 'us-east-1a', 'standard', 'version-1', 1)`); err != nil {
		t.Fatalf("insert foreign Environment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO capsule_locks (id, environment_id, profile_version_id, project_capsule_digest, digest, capsules, resolved_components, created_at)
		VALUES ('lock-foreign', 'environment-2', 'version-1', $1, $2, '[]', '{}', $3)`, testDigest('b'), testDigest('c'), now); err != nil {
		t.Fatalf("insert foreign Capsule Lock: %v", err)
	}
	lockID := "lock-foreign"
	if _, err := store.UpsertEnvironmentPin(ctx, dbstore.EnvironmentPinInput{
		EnvironmentID: "environment-1", CapsuleLockID: &lockID, UpgradePolicy: domain.UpgradeManual,
	}); err == nil {
		t.Fatal("UpsertEnvironmentPin() accepted a foreign Capsule Lock")
	}
}

func TestReplaceEnvironmentMaterializationsRejectsForeignLockAndPreservesExistingRows(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 12, 45, 0, 0, time.UTC)
	seedEnvironmentMaterializationPrerequisites(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version)
		VALUES ('environment-2', 'user-1', 'Other', 'other', 'creating', 'unknown', 'us-east-1', 'us-east-1a', 'standard', 'version-1', 1)`); err != nil {
		t.Fatalf("insert foreign Environment: %v", err)
	}
	capsuleDigest := testDigest('a')
	componentDigest := testDigest('b')
	newLock := func(id, environmentID, lockedCapsuleDigest, lockedComponentDigest string, createdAt time.Time) domain.CapsuleLock {
		lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
			ID: id, EnvironmentID: environmentID, ProfileVersionID: "version-1", ProjectCapsuleDigest: lockedCapsuleDigest,
			Capsules: []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + lockedCapsuleDigest, Digest: lockedCapsuleDigest}},
			ResolvedComponents: map[string]domain.ResolvedComponent{
				"config:editor": {
					ID: "config:editor", Type: domain.ComponentConfig, CapsuleDigest: lockedCapsuleDigest,
					ComponentDigest: lockedComponentDigest, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
				},
			},
			CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if _, err := store.PersistCapsuleLock(ctx, lock); err != nil {
			t.Fatalf("persist %s: %v", id, err)
		}
		return lock
	}
	environmentOneLock := newLock("lock-environment-1", "environment-1", capsuleDigest, componentDigest, now)
	environmentTwoCapsuleDigest := testDigest('c')
	environmentTwoComponentDigest := testDigest('d')
	environmentTwoLock := newLock("lock-environment-2", "environment-2", environmentTwoCapsuleDigest, environmentTwoComponentDigest, now.Add(time.Minute))

	existing := environmentMaterializationFixture("config:editor", environmentOneLock.Snapshot().ID, now)
	existing.LockDigest = environmentOneLock.Snapshot().Digest
	existing.CapsuleDigest = capsuleDigest
	existing.ComponentDigest = componentDigest
	if err := store.UpsertEnvironmentMaterializations(ctx, []dbstore.EnvironmentMaterialization{existing}); err != nil {
		t.Fatalf("seed Environment-1 Materialization: %v", err)
	}
	foreign := existing
	foreign.EnvironmentID = "environment-2"
	foreign.LockID = environmentTwoLock.Snapshot().ID
	foreign.LockDigest = environmentTwoLock.Snapshot().Digest
	foreign.CapsuleDigest = environmentTwoCapsuleDigest
	foreign.ComponentDigest = environmentTwoComponentDigest
	foreign.ObservedDigest = testDigest('9')

	if err := store.ReplaceEnvironmentMaterializationsForLock(ctx, "environment-1", environmentTwoLock.Snapshot().ID, []dbstore.EnvironmentMaterialization{foreign}); err == nil {
		t.Fatal("ReplaceEnvironmentMaterializationsForLock() accepted an Environment-2 lock and record for Environment-1")
	}
	loaded, err := store.ListEnvironmentMaterializations(ctx, "environment-1")
	if err != nil {
		t.Fatalf("load Environment-1 Materializations after rejection: %v", err)
	}
	if len(loaded) != 1 || loaded[0].LockID != environmentOneLock.Snapshot().ID || loaded[0].ObservedDigest != existing.ObservedDigest {
		t.Fatalf("Environment-1 Materializations after foreign replace = %#v, want original row", loaded)
	}
}

func TestStoreRoundTripsMaterializationWithoutObservedState(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 13, 0, 0, 0, time.UTC)
	seedEnvironmentMaterializationPrerequisites(t, ctx, pool)

	lock := persistEnvironmentMaterializationLockFixture(
		t, ctx, store, "lock-empty-state", "environment-1", "version-1", testDigest('b'), now,
		"config:editor",
	)
	record := environmentMaterializationFixture("config:editor", "lock-empty-state", now)
	record.LockDigest = lock.Snapshot().Digest
	record.LastAppliedDigest = ""
	record.ObservedDigest = ""
	record.CredentialRequirementDigest = ""
	if err := store.UpsertEnvironmentMaterializations(ctx, []dbstore.EnvironmentMaterialization{record}); err != nil {
		t.Fatalf("UpsertEnvironmentMaterializations(): %v", err)
	}

	loaded, err := store.ListEnvironmentMaterializations(ctx, "environment-1")
	if err != nil {
		t.Fatalf("ListEnvironmentMaterializations(): %v", err)
	}
	if len(loaded) != 1 || loaded[0].LastAppliedDigest != "" || loaded[0].ObservedDigest != "" || loaded[0].CredentialRequirementDigest != "" {
		t.Fatalf("round-tripped optional state = %#v", loaded)
	}
}

func TestStorePersistsCapsuleLockPinAndMaterializationsAtomically(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 14, 0, 0, 0, time.UTC)
	insertCreationPrerequisites(t, ctx, pool)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{"name":"workspace"}`), now)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	digest := testDigest('a')
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: "lock-atomic", EnvironmentID: "environment-1", ProfileVersionID: "profile-version-1", ProjectCapsuleDigest: digest,
		Capsules: []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + digest, Digest: digest}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {ID: "config:editor", Type: domain.ComponentConfig, CapsuleDigest: digest, ComponentDigest: digest, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative},
		},
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create atomic Capsule Lock: %v", err)
	}
	record := environmentMaterializationFixture("config:editor", "lock-atomic", now)
	record.LockDigest = lock.Snapshot().Digest
	record.CapsuleDigest = digest
	record.ComponentDigest = digest
	if err := store.PersistEnvironmentCapsuleState(ctx, dbstore.EnvironmentCapsuleStateInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", CapsuleLock: lock, UpgradePolicy: domain.UpgradeAutoSafe, Materializations: []dbstore.EnvironmentMaterialization{record},
	}); err != nil {
		t.Fatalf("PersistEnvironmentCapsuleState(): %v", err)
	}
	pin, err := store.GetEnvironmentPin(ctx, "environment-1")
	if err != nil {
		t.Fatalf("GetEnvironmentPin() after atomic persist: %v", err)
	}
	if pin.CapsuleLockID == nil || *pin.CapsuleLockID != "lock-atomic" || pin.UpgradePolicy != domain.UpgradeAutoSafe {
		t.Fatalf("atomic Environment pin = %#v", pin)
	}
	materializations, err := store.ListEnvironmentMaterializations(ctx, "environment-1")
	if err != nil {
		t.Fatalf("ListEnvironmentMaterializations() after atomic persist: %v", err)
	}
	if len(materializations) != 1 || materializations[0].LockID != "lock-atomic" {
		t.Fatalf("atomic materializations = %#v", materializations)
	}
}

func TestPersistEnvironmentCapsuleStateRejectsLockOwnedByDifferentEnvironmentThanOperation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 14, 30, 0, 0, time.UTC)
	insertCreationPrerequisites(t, ctx, pool)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{"name":"workspace"}`), now)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version)
		VALUES ('environment-2', 'user-1', 'Other', 'other', 'creating', 'unknown', 'us-east-1', 'us-east-1a', 'standard', 'profile-version-1', 1)`); err != nil {
		t.Fatalf("insert foreign Environment: %v", err)
	}
	digest := testDigest('a')
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: "lock-foreign-operation", EnvironmentID: "environment-2", ProfileVersionID: "profile-version-1", ProjectCapsuleDigest: digest,
		Capsules:           []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + digest, Digest: digest}},
		ResolvedComponents: map[string]domain.ResolvedComponent{}, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create foreign Capsule Lock: %v", err)
	}

	err = store.PersistEnvironmentCapsuleState(ctx, dbstore.EnvironmentCapsuleStateInput{
		OperationID: "operation-1", EnvironmentID: "environment-2", CapsuleLock: lock, UpgradePolicy: domain.UpgradeManual,
	})
	if err == nil {
		t.Fatal("PersistEnvironmentCapsuleState() accepted a lock owned by a different Environment than the persisted Operation")
	}
	var locks, pins int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capsule_locks WHERE id = 'lock-foreign-operation'`).Scan(&locks); err != nil {
		t.Fatalf("count foreign-operation Capsule Locks: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM environments WHERE id = 'environment-2' AND capsule_lock_id IS NOT NULL`).Scan(&pins); err != nil {
		t.Fatalf("count foreign-operation Environment pins: %v", err)
	}
	if locks != 0 || pins != 0 {
		t.Fatalf("foreign-operation Capsule state writes = locks:%d pins:%d, want none", locks, pins)
	}
}

func TestPersistEnvironmentCapsuleStateRejectsMaterializationIdentityForgedFromLock(t *testing.T) {
	for _, test := range []struct {
		name  string
		forge func(*dbstore.EnvironmentMaterialization)
	}{
		{name: "trust class", forge: func(record *dbstore.EnvironmentMaterialization) { record.TrustClass = domain.TrustDeclarative }},
		{name: "component digest", forge: func(record *dbstore.EnvironmentMaterialization) { record.ComponentDigest = testDigest('f') }},
		{name: "lock digest", forge: func(record *dbstore.EnvironmentMaterialization) { record.LockDigest = testDigest('f') }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, pool := openTestStoreAndPool(t, ctx)
			now := time.Date(2026, time.July, 17, 14, 45, 0, 0, time.UTC)
			insertCreationPrerequisites(t, ctx, pool)
			creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{"name":"workspace"}`), now)
			if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
				t.Fatalf("reserve Environment creation: %v", err)
			}
			capsuleDigest := testDigest('a')
			componentDigest := testDigest('b')
			lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
				ID: "lock-forged-materialization", EnvironmentID: "environment-1", ProfileVersionID: "profile-version-1", ProjectCapsuleDigest: capsuleDigest,
				Capsules: []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + capsuleDigest, Digest: capsuleDigest}},
				ResolvedComponents: map[string]domain.ResolvedComponent{
					"command:build": {
						ID: "command:build", Type: domain.ComponentCommand, CapsuleDigest: capsuleDigest,
						ComponentDigest: componentDigest, Scope: domain.ScopeProject, TrustClass: domain.TrustExecutable,
					},
				},
				CreatedAt: now,
			})
			if err != nil {
				t.Fatalf("create Capsule Lock: %v", err)
			}
			record := environmentMaterializationFixture("command:build", lock.Snapshot().ID, now)
			record.LockDigest = lock.Snapshot().Digest
			record.CapsuleDigest = capsuleDigest
			record.ComponentDigest = componentDigest
			record.Scope = domain.ScopeProject
			record.ComponentType = domain.ComponentCommand
			record.TrustClass = domain.TrustExecutable
			test.forge(&record)

			err = store.PersistEnvironmentCapsuleState(ctx, dbstore.EnvironmentCapsuleStateInput{
				OperationID: "operation-1", EnvironmentID: "environment-1", CapsuleLock: lock,
				UpgradePolicy: domain.UpgradeManual, Materializations: []dbstore.EnvironmentMaterialization{record},
			})
			if err == nil {
				t.Fatalf("PersistEnvironmentCapsuleState() accepted forged %s", test.name)
			}
			var locks, materializations int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM capsule_locks WHERE id = 'lock-forged-materialization'`).Scan(&locks); err != nil {
				t.Fatalf("count forged-state Capsule Locks: %v", err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM environment_materializations`).Scan(&materializations); err != nil {
				t.Fatalf("count forged Materializations: %v", err)
			}
			if locks != 0 || materializations != 0 {
				t.Fatalf("forged %s writes = locks:%d materializations:%d, want none", test.name, locks, materializations)
			}
		})
	}
}

func TestStoreRollsBackCapsuleStateWhenMaterializationPersistenceFails(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 15, 0, 0, 0, time.UTC)
	insertCreationPrerequisites(t, ctx, pool)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{"name":"workspace"}`), now)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	digest := testDigest('a')
	lock := environmentMaterializationLockFixture(
		t, "lock-rollback", "environment-1", "profile-version-1", digest, now,
		"config:editor",
	)
	invalid := environmentMaterializationFixture("config:editor", "lock-rollback", now)
	invalid.LockDigest = lock.Snapshot().Digest
	invalid.CapsuleDigest = digest
	invalid.EffectiveCacheKey = ""
	if err := store.PersistEnvironmentCapsuleState(ctx, dbstore.EnvironmentCapsuleStateInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", CapsuleLock: lock,
		UpgradePolicy: domain.UpgradeNotify, Materializations: []dbstore.EnvironmentMaterialization{invalid},
	}); err == nil {
		t.Fatal("PersistEnvironmentCapsuleState() accepted invalid Materialization")
	}
	var locks, pins int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capsule_locks WHERE id = 'lock-rollback'`).Scan(&locks); err != nil {
		t.Fatalf("count rolled-back Capsule Lock: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM environments WHERE id = 'environment-1' AND capsule_lock_id IS NOT NULL`).Scan(&pins); err != nil {
		t.Fatalf("count rolled-back Environment pin: %v", err)
	}
	if locks != 0 || pins != 0 {
		t.Fatalf("rolled-back Capsule state = locks:%d pins:%d", locks, pins)
	}
}

func seedEnvironmentMaterializationPrerequisites(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	statements := []string{
		`INSERT INTO users (id, workos_user_id, default_region) VALUES ('user-1', 'workos-1', 'us-east-1')`,
		`INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-1', 'user-1', 'Default', 'default')`,
		`INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('version-1', 'profile-1', 1, 'sha256:' || repeat('a', 64))`,
		`INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version) VALUES ('environment-1', 'user-1', 'Workspace', 'workspace', 'creating', 'unknown', 'us-east-1', 'us-east-1a', 'standard', 'version-1', 1)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("insert Environment pin prerequisite: %v", err)
		}
	}
}

func environmentMaterializationFixture(componentID, lockID string, at time.Time) dbstore.EnvironmentMaterialization {
	return dbstore.EnvironmentMaterialization{
		EnvironmentID: "environment-1", ID: componentID, LockID: lockID, LockDigest: testDigest('c'), CapsuleDigest: testDigest('b'),
		ComponentID: componentID, ComponentDigest: testDigest('d'), AdapterID: "file", AdapterVersion: "v1", TargetAgentVersion: "agent-1",
		Scope: domain.ScopeUser, ComponentType: domain.ComponentConfig, TrustClass: domain.TrustDeclarative,
		NonSecretOverridesDigest: testDigest('0'), SecretVersionIdentifiers: []string{"secret-v1", "secret-v2"}, EffectiveCacheKey: testDigest('e'),
		Mode: "managed", Root: "home", Target: ".config/editor.json", Selector: "$", Directory: true, FilePaths: []string{"editor.json", "settings.json"},
		LastAppliedDigest: testDigest('f'), ObservedDigest: testDigest('f'), CredentialRequirementDigest: testDigest('1'), CreatedAt: at, UpdatedAt: at,
	}
}

func environmentMaterializationLockFixture(t *testing.T, lockID, environmentID, profileVersionID, capsuleDigest string, at time.Time, componentIDs ...string) domain.CapsuleLock {
	t.Helper()
	components := make(map[string]domain.ResolvedComponent, len(componentIDs))
	for _, componentID := range componentIDs {
		components[componentID] = domain.ResolvedComponent{
			ID: componentID, Type: domain.ComponentConfig, CapsuleDigest: capsuleDigest,
			ComponentDigest: testDigest('d'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
		}
	}
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: lockID, EnvironmentID: environmentID, ProfileVersionID: profileVersionID, ProjectCapsuleDigest: capsuleDigest,
		Capsules:           []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + capsuleDigest, Digest: capsuleDigest}},
		ResolvedComponents: components, CreatedAt: at.UTC(),
	})
	if err != nil {
		t.Fatalf("create Capsule Lock fixture %q: %v", lockID, err)
	}
	return lock
}

func persistEnvironmentMaterializationLockFixture(t *testing.T, ctx context.Context, store *dbstore.Store, lockID, environmentID, profileVersionID, capsuleDigest string, at time.Time, componentIDs ...string) domain.CapsuleLock {
	t.Helper()
	lock := environmentMaterializationLockFixture(t, lockID, environmentID, profileVersionID, capsuleDigest, at, componentIDs...)
	persisted, err := store.PersistCapsuleLock(ctx, lock)
	if err != nil {
		t.Fatalf("persist Capsule Lock fixture %q: %v", lockID, err)
	}
	return persisted
}

func testDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}
