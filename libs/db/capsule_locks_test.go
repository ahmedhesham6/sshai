package db_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStorePersistsCapsuleLockIdempotentlyAndImmutably(t *testing.T) {
	ctx := t.Context()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)
	if _, err := pool.Exec(ctx, `INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-1', 'user-1', 'Personal', 'personal')`); err != nil {
		t.Fatalf("insert Profile: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('version-1', 'profile-1', 1, 'sha256:' || repeat('a', 64))`); err != nil {
		t.Fatalf("insert Profile Version: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version) VALUES ('environment-1', 'user-1', 'Workspace', 'workspace', 'active', 'healthy', 'us-east-1', 'us-east-1a', 'standard', 'version-1', 1)`); err != nil {
		t.Fatalf("insert Environment: %v", err)
	}

	capsuleDigest := "sha256:" + strings.Repeat("b", 64)
	snapshot := domain.CapsuleLockSnapshot{
		ID: "lock-1", EnvironmentID: "environment-1", ProfileVersionID: "version-1", ProjectCapsuleDigest: "sha256:" + strings.Repeat("c", 64),
		Capsules: []domain.LockedCapsule{{Ref: "registry.example.com/team/base:stable", Digest: capsuleDigest}},
		ResolvedComponents: map[string]domain.ResolvedComponent{"config:editor": {
			ID: "config:editor", CapsuleDigest: capsuleDigest, ComponentDigest: "sha256:" + strings.Repeat("d", 64), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
		}}, CreatedAt: now,
	}
	lock, err := domain.CreateCapsuleLock(snapshot)
	if err != nil {
		t.Fatalf("create Capsule Lock fixture: %v", err)
	}
	saved, err := store.PersistCapsuleLock(ctx, lock)
	if err != nil {
		t.Fatalf("PersistCapsuleLock(): %v", err)
	}
	replayed, err := store.PersistCapsuleLock(ctx, lock)
	if err != nil {
		t.Fatalf("PersistCapsuleLock() replay: %v", err)
	}
	if saved.Snapshot().ID != "lock-1" || replayed.Snapshot().ID != saved.Snapshot().ID || replayed.Snapshot().Digest != saved.Snapshot().Digest {
		t.Fatalf("saved/replayed locks = %#v / %#v", saved.Snapshot(), replayed.Snapshot())
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capsule_locks WHERE environment_id = 'environment-1'`).Scan(&count); err != nil {
		t.Fatalf("count Capsule Locks: %v", err)
	}
	if count != 1 {
		t.Fatalf("Capsule Lock rows = %d, want 1", count)
	}
	if _, err := pool.Exec(ctx, `UPDATE capsule_locks SET digest = 'sha256:' || repeat('e', 64) WHERE id = 'lock-1'`); err == nil {
		t.Fatal("Capsule Lock update succeeded, want immutable storage")
	}
}

func TestStorePersistsConcurrentSameTargetCapsuleLocksIdempotently(t *testing.T) {
	ctx := t.Context()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 16, 12, 30, 0, 0, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)
	statements := []string{
		`INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-concurrent', 'user-1', 'Concurrent', 'concurrent')`,
		`INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('version-concurrent', 'profile-concurrent', 1, 'sha256:' || repeat('a', 64))`,
		`INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version) VALUES ('environment-concurrent', 'user-1', 'Concurrent', 'concurrent', 'active', 'healthy', 'us-east-1', 'us-east-1a', 'standard', 'version-concurrent', 1)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("insert concurrent Capsule Lock prerequisite: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION test_capsule_lock_insert_delay() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_sleep(0.25);
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER test_capsule_lock_insert_delay
		BEFORE INSERT ON capsule_locks
		FOR EACH ROW EXECUTE FUNCTION test_capsule_lock_insert_delay();`); err != nil {
		t.Fatalf("install concurrent Capsule Lock barrier: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_capsule_lock_insert_delay ON capsule_locks`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_capsule_lock_insert_delay()`)
	})
	capsuleDigest := "sha256:" + strings.Repeat("b", 64)
	componentDigest := "sha256:" + strings.Repeat("c", 64)
	newCandidate := func(id string) domain.CapsuleLock {
		candidate, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
			ID: "lock-concurrent-" + id, EnvironmentID: "environment-concurrent", ProfileVersionID: "version-concurrent",
			ProjectCapsuleDigest: "sha256:" + strings.Repeat("d", 64),
			Capsules:             []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + capsuleDigest, Digest: capsuleDigest}},
			ResolvedComponents: map[string]domain.ResolvedComponent{"config:editor": {
				ID: "config:editor", CapsuleDigest: capsuleDigest, ComponentDigest: componentDigest,
				Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
			}},
			CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("create concurrent Capsule Lock fixture: %v", err)
		}
		return candidate
	}
	candidates := []domain.CapsuleLock{newCandidate("one"), newCandidate("two")}
	start := make(chan struct{})
	results := make(chan struct {
		lock domain.CapsuleLock
		err  error
	}, len(candidates))
	var group sync.WaitGroup
	for _, candidate := range candidates {
		group.Add(1)
		go func(candidate domain.CapsuleLock) {
			defer group.Done()
			<-start
			lock, err := store.PersistCapsuleLock(ctx, candidate)
			results <- struct {
				lock domain.CapsuleLock
				err  error
			}{lock: lock, err: err}
		}(candidate)
	}
	close(start)
	group.Wait()
	close(results)
	var first domain.CapsuleLock
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent PersistCapsuleLock() error: %v", result.err)
		}
		if first.Snapshot().ID == "" {
			first = result.lock
			continue
		}
		if result.lock.Snapshot().ID != first.Snapshot().ID || result.lock.Snapshot().Digest != first.Snapshot().Digest {
			t.Fatalf("concurrent locks = %#v and %#v, want same persisted lock", first.Snapshot(), result.lock.Snapshot())
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capsule_locks WHERE environment_id = 'environment-concurrent'`).Scan(&count); err != nil {
		t.Fatalf("count concurrent Capsule Locks: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent Capsule Lock rows = %d, want 1", count)
	}
}

func TestStoreProjectsProfileResolveOperationAndCompletesItIdempotently(t *testing.T) {
	ctx := t.Context()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 16, 13, 0, 0, 0, time.UTC)
	ensureProfileUser(t, ctx, store, "user-1", now)
	statements := []string{
		`INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-1', 'user-1', 'Personal', 'personal')`,
		`INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('version-1', 'profile-1', 1, 'sha256:' || repeat('a', 64))`,
		`INSERT INTO environments (id, owner_user_id, name, slug, lifecycle, health, region, availability_zone, runtime_preset, pinned_profile_version_id, version) VALUES ('environment-1', 'user-1', 'Workspace', 'workspace', 'active', 'healthy', 'us-east-1', 'us-east-1a', 'standard', 'version-1', 1)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("insert Profile resolve prerequisite: %v", err)
		}
	}
	if err := store.RecordProfileResolveInvocation(ctx, "operation-1", "invocation-1", "environment-1", "version-1", nil, now); err != nil {
		t.Fatalf("RecordProfileResolveInvocation(): %v", err)
	}
	if err := store.RecordProfileResolveInvocation(ctx, "operation-1", "invocation-1", "environment-1", "version-1", nil, now.Add(time.Minute)); err != nil {
		t.Fatalf("RecordProfileResolveInvocation() replay: %v", err)
	}
	if err := store.CompleteProfileResolve(ctx, "operation-1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("CompleteProfileResolve(): %v", err)
	}
	if err := store.CompleteProfileResolve(ctx, "operation-1", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("CompleteProfileResolve() replay: %v", err)
	}
	var operationStatus, stepStatus string
	var operationCount, stepCount int
	if err := pool.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM operations WHERE id = 'operation-1'), (SELECT count(*) FROM operation_steps WHERE operation_id = 'operation-1') FROM operations WHERE id = 'operation-1'`).Scan(&operationStatus, &operationCount, &stepCount); err != nil {
		t.Fatalf("read projected Profile resolve Operation: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM operation_steps WHERE operation_id = 'operation-1' AND step_key = 'resolve'`).Scan(&stepStatus); err != nil {
		t.Fatalf("read projected Profile resolve Step: %v", err)
	}
	if operationStatus != "succeeded" || stepStatus != "succeeded" || operationCount != 1 || stepCount != 1 {
		t.Fatalf("Profile resolve projection = operation:%s step:%s rows:%d/%d", operationStatus, stepStatus, operationCount, stepCount)
	}
}
