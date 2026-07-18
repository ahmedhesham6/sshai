package db_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreGetsOwnedEnvironmentBeforeRuntimeOrCapsuleLockExist(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}

	detail, err := store.GetOwnedEnvironment(ctx, "user-1", "environment-1")
	if err != nil {
		t.Fatalf("get owned Environment: %v", err)
	}
	snapshot := detail.Environment.Snapshot()
	if snapshot.ID != "environment-1" || snapshot.Lifecycle != domain.EnvironmentCreating {
		t.Fatalf("Environment = %#v", snapshot)
	}
	if snapshot.CapsuleLockID != nil {
		t.Fatalf("Capsule Lock ID = %v, want nil before resolve", snapshot.CapsuleLockID)
	}
	if detail.AutoStopMode != domain.AutoStopManual || detail.GracePeriodSeconds != 0 {
		t.Fatalf("Auto-stop Policy = mode:%s grace:%d", detail.AutoStopMode, detail.GracePeriodSeconds)
	}
	if detail.Runtime != nil {
		t.Fatalf("Runtime = %#v, want nil before provisioning", detail.Runtime)
	}
	if detail.CapsuleLock != nil {
		t.Fatalf("Capsule Lock = %#v, want nil before resolve", detail.CapsuleLock)
	}
	if detail.ActiveOperationID == nil || *detail.ActiveOperationID != "operation-1" {
		t.Fatalf("active Operation ID = %v, want operation-1", detail.ActiveOperationID)
	}
}

func TestStoreGetsOwnedEnvironmentWithRuntimeAndCapsuleLock(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	if _, err := store.RecordEnvironmentCreateInvocation(ctx, "operation-1", "invocation-1", createdAt.Add(time.Second)); err != nil {
		t.Fatalf("record Environment create invocation: %v", err)
	}
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", environmentStateReservation(createdAt.Add(30*time.Second), "volume-1")); err != nil {
		t.Fatalf("inventory Environment State: %v", err)
	}
	if _, err := store.ReserveInitialRuntime(ctx, "operation-1", initialRuntimeReservation(createdAt.Add(45*time.Second))); err != nil {
		t.Fatalf("reserve initial Runtime: %v", err)
	}
	completedAt := createdAt.Add(time.Minute)
	if _, err := store.CompleteEnvironmentCreation(ctx, "operation-1", completedAt); err != nil {
		t.Fatalf("complete Environment creation: %v", err)
	}
	insertCapsuleLock(t, ctx, pool, "lock-1", "environment-1", "profile-version-1", completedAt)
	if _, err := pool.Exec(ctx, `UPDATE environments SET capsule_lock_id = 'lock-1' WHERE id = 'environment-1'`); err != nil {
		t.Fatalf("pin Capsule Lock: %v", err)
	}

	detail, err := store.GetOwnedEnvironment(ctx, "user-1", "environment-1")
	if err != nil {
		t.Fatalf("get owned Environment: %v", err)
	}
	snapshot := detail.Environment.Snapshot()
	if snapshot.Lifecycle != domain.EnvironmentActive || snapshot.Health != domain.EnvironmentHealthHealthy {
		t.Fatalf("Environment = %#v", snapshot)
	}
	if detail.ActiveOperationID != nil {
		t.Fatalf("active Operation ID = %v, want nil once completed", detail.ActiveOperationID)
	}
	if detail.Runtime == nil || detail.Runtime.Snapshot().ID != "runtime-1" {
		t.Fatalf("Runtime = %#v, want runtime-1", detail.Runtime)
	}
	if detail.CapsuleLock == nil || detail.CapsuleLock.Snapshot().ID != "lock-1" {
		t.Fatalf("Capsule Lock = %#v, want lock-1", detail.CapsuleLock)
	}
}

func TestStoreRejectsForeignOrAbsentOwnedEnvironment(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}

	if _, err := store.GetOwnedEnvironment(ctx, "user-2", "environment-1"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign owner Get error = %v, want ErrReferenceNotOwned", err)
	}
	if _, err := store.GetOwnedEnvironment(ctx, "user-1", "missing-environment"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("absent Environment Get error = %v, want ErrReferenceNotOwned", err)
	}
}

func TestStoreListsOnlyOwnedEnvironmentsOrderedByCreation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-2', 'user-2', 'Default', 'default')`); err != nil {
		t.Fatalf("insert foreign Profile: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('profile-version-2', 'profile-2', 1, 'sha256:' || repeat('d', 64))`); err != nil {
		t.Fatalf("insert foreign Profile Version: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, manifest_digest) VALUES ('project-seed-2', 'user-1', 'https://github.com/example/project.git', 'abc123', 'sha256:' || repeat('1', 64), 'sha256:' || repeat('2', 64))`); err != nil {
		t.Fatalf("insert second Project Seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, manifest_digest) VALUES ('project-seed-foreign', 'user-2', 'https://github.com/example/project.git', 'abc123', 'sha256:' || repeat('3', 64), 'sha256:' || repeat('4', 64))`); err != nil {
		t.Fatalf("insert foreign Project Seed: %v", err)
	}
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

	first := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, first); err != nil {
		t.Fatalf("reserve first Environment: %v", err)
	}
	second, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-2", OwnerUserID: "user-1", Name: "Second", Slug: "second", Region: "us-east-1",
		AvailabilityZone: "us-east-1a", RuntimePreset: "standard", PinnedProfileVersionID: "profile-version-1",
		AutoStopPolicyID: "policy-2", CreatedAt: createdAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve second domain Environment: %v", err)
	}
	policy, err := domain.NewAutoStopPolicy("policy-2", "environment-2", domain.AutoStopManual, 0)
	if err != nil {
		t.Fatalf("create second Auto-stop Policy: %v", err)
	}
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: "operation-2", EnvironmentID: "environment-2", Type: domain.OperationEnvironmentCreate,
		RequestedByUserID: "user-1", IdempotencyKey: "request-key-0002", Input: []byte(`{}`), CreatedAt: createdAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("queue second Operation: %v", err)
	}
	secondCreation, err := domain.NewEnvironmentCreation(second, policy, operation, "project-seed-2", []string{"ssh-key-1"})
	if err != nil {
		t.Fatalf("create second Environment reservation: %v", err)
	}
	if _, err := store.ReserveEnvironmentCreation(ctx, secondCreation); err != nil {
		t.Fatalf("reserve second Environment: %v", err)
	}
	foreign, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-foreign", OwnerUserID: "user-2", Name: "Foreign", Slug: "foreign", Region: "us-east-1",
		AvailabilityZone: "us-east-1a", RuntimePreset: "standard", PinnedProfileVersionID: "profile-version-2",
		AutoStopPolicyID: "policy-foreign", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("reserve foreign domain Environment: %v", err)
	}
	foreignPolicy, err := domain.NewAutoStopPolicy("policy-foreign", "environment-foreign", domain.AutoStopManual, 0)
	if err != nil {
		t.Fatalf("create foreign Auto-stop Policy: %v", err)
	}
	foreignOperation, err := domain.QueueOperation(domain.OperationRequest{
		ID: "operation-foreign", EnvironmentID: "environment-foreign", Type: domain.OperationEnvironmentCreate,
		RequestedByUserID: "user-2", IdempotencyKey: "request-key-foreign", Input: []byte(`{}`), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("queue foreign Operation: %v", err)
	}
	foreignCreation, err := domain.NewEnvironmentCreation(foreign, foreignPolicy, foreignOperation, "project-seed-foreign", []string{"foreign-key"})
	if err != nil {
		t.Fatalf("create foreign Environment reservation: %v", err)
	}
	if _, err := store.ReserveEnvironmentCreation(ctx, foreignCreation); err != nil {
		t.Fatalf("reserve foreign Environment: %v", err)
	}

	details, nextCursor, err := store.ListOwnedEnvironments(ctx, "user-1", nil, 0)
	if err != nil {
		t.Fatalf("list owned Environments: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("owned Environment count = %d, want 2", len(details))
	}
	if details[0].Environment.Snapshot().ID != "environment-1" || details[1].Environment.Snapshot().ID != "environment-2" {
		t.Fatalf("owned Environment order = %#v", []string{details[0].Environment.Snapshot().ID, details[1].Environment.Snapshot().ID})
	}
	if nextCursor != nil {
		t.Fatalf("next cursor = %#v, want nil once every owned Environment fits on the page", nextCursor)
	}
}

// TestStorePaginatesOwnedEnvironmentsWithStableKeysetWalk confirms the
// keyset contract Finding 1 requires: paging through with a small page size
// visits every owned Environment exactly once, in creation order, with no
// overlap or gaps, and replaying the same request is stable.
func TestStorePaginatesOwnedEnvironmentsWithStableKeysetWalk(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	// project_seeds.environment_id is UNIQUE, so every Environment needs its
	// own Project Seed; project-seed-1 already exists via
	// insertCreationPrerequisites for environment-1.
	for index := 2; index <= 5; index++ {
		suffix := fmt.Sprintf("%d", index)
		if _, err := pool.Exec(ctx, `INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, manifest_digest) VALUES ($1, 'user-1', 'https://github.com/example/project.git', 'abc123', $2, $3)`,
			"project-seed-"+suffix, digest(byte('0'+index)), digest('b')); err != nil {
			t.Fatalf("insert Project Seed %d: %v", index, err)
		}
	}
	for index := 0; index < 5; index++ {
		suffix := fmt.Sprintf("%d", index+1)
		creation := newEnvironmentCreationWithSeed(t, "environment-"+suffix, "policy-"+suffix, "operation-"+suffix,
			"project-seed-"+suffix, "workspace-"+suffix, "request-key-"+suffix, []byte(`{}`), createdAt.Add(time.Duration(index)*time.Minute))
		if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
			t.Fatalf("reserve Environment %d: %v", index, err)
		}
	}

	var cursor *dbstore.Cursor
	var seen []string
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paginated more than 10 times walking 5 Environments with page size 2; likely stuck in a loop")
		}
		details, next, err := store.ListOwnedEnvironments(ctx, "user-1", cursor, 2)
		if err != nil {
			t.Fatalf("list owned Environments page %d: %v", pages, err)
		}
		for _, detail := range details {
			seen = append(seen, detail.Environment.Snapshot().ID)
		}
		if next == nil {
			break
		}
		cursor = next
	}
	want := []string{"environment-1", "environment-2", "environment-3", "environment-4", "environment-5"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("paginated owned Environment IDs = %#v, want %#v", seen, want)
	}

	replay, replayNext, err := store.ListOwnedEnvironments(ctx, "user-1", nil, 2)
	if err != nil {
		t.Fatalf("replay first page: %v", err)
	}
	if len(replay) != 2 || replay[0].Environment.Snapshot().ID != "environment-1" || replay[1].Environment.Snapshot().ID != "environment-2" {
		t.Fatalf("replayed first page = %#v", replay)
	}
	if replayNext == nil {
		t.Fatal("replayed first page next cursor = nil, want non-nil (3 Environments remain)")
	}
}

// TestStorePaginatesOwnedEnvironmentsDisambiguatingIdenticalCreatedAt
// confirms the keyset boundary case: Environments sharing the exact same
// created_at are still split across pages deterministically, ordered and
// disambiguated by id, with neither a skip nor a duplicate at the boundary.
func TestStorePaginatesOwnedEnvironmentsDisambiguatingIdenticalCreatedAt(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	for _, suffix := range []string{"2", "3"} {
		if _, err := pool.Exec(ctx, `INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, manifest_digest) VALUES ($1, 'user-1', 'https://github.com/example/project.git', 'abc123', $2, $3)`,
			"project-seed-"+suffix, digest(suffix[0]), digest('b')); err != nil {
			t.Fatalf("insert Project Seed %s: %v", suffix, err)
		}
	}
	// All three Environments share the same created_at; only id can order them.
	for _, spec := range []struct{ id, projectSeedID string }{
		{"environment-a", "project-seed-1"},
		{"environment-b", "project-seed-2"},
		{"environment-c", "project-seed-3"},
	} {
		creation := newEnvironmentCreationWithSeed(t, spec.id, "policy-"+spec.id, "operation-"+spec.id, spec.projectSeedID, "workspace-"+spec.id, "request-key-"+spec.id, []byte(`{}`), createdAt)
		if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
			t.Fatalf("reserve Environment %q: %v", spec.id, err)
		}
	}

	first, cursor, err := store.ListOwnedEnvironments(ctx, "user-1", nil, 2)
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if len(first) != 2 || first[0].Environment.Snapshot().ID != "environment-a" || first[1].Environment.Snapshot().ID != "environment-b" {
		t.Fatalf("first page = %#v, want [environment-a environment-b] ordered by id under a shared created_at", first)
	}
	if cursor == nil || cursor.ID != "environment-b" {
		t.Fatalf("cursor after first page = %#v, want it to key off environment-b", cursor)
	}

	second, secondCursor, err := store.ListOwnedEnvironments(ctx, "user-1", cursor, 2)
	if err != nil {
		t.Fatalf("list second page: %v", err)
	}
	if len(second) != 1 || second[0].Environment.Snapshot().ID != "environment-c" {
		t.Fatalf("second page = %#v, want exactly [environment-c] (no duplicate, no skip at the boundary)", second)
	}
	if secondCursor != nil {
		t.Fatalf("cursor after final page = %#v, want nil", secondCursor)
	}
}

func insertCapsuleLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, environmentID, profileVersionID string, createdAt time.Time) {
	t.Helper()
	projectDigest := digest('f')
	capsules := []domain.LockedCapsule{{Ref: "owner/example/capsule@" + digest('a'), Digest: digest('b')}}
	snapshot := domain.CapsuleLockSnapshot{
		ID: id, EnvironmentID: environmentID, ProfileVersionID: profileVersionID,
		ProjectCapsuleDigest: projectDigest, Capsules: capsules, CreatedAt: createdAt.UTC(),
	}
	lockDigest := domain.ComputeCapsuleLockDigest(snapshot)
	if _, err := pool.Exec(ctx, `
		INSERT INTO capsule_locks (id, environment_id, profile_version_id, project_capsule_digest, digest, capsules, resolved_components, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, environmentID, profileVersionID, projectDigest, lockDigest,
		`[{"ref":"owner/example/capsule@`+digest('a')+`","digest":"`+digest('b')+`"}]`,
		`{}`, createdAt); err != nil {
		t.Fatalf("insert Capsule Lock: %v", err)
	}
}
