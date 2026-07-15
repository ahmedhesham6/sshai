package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreEnsuresStableUserProjection(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	firstSeen := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

	first, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID:            "usr_01",
		WorkOSUserID:  "workos_01",
		DefaultRegion: "us-east-1",
		ObservedAt:    firstSeen,
	})
	if err != nil {
		t.Fatalf("ensure first User: %v", err)
	}
	second, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID:            "usr_replacement",
		WorkOSUserID:  "workos_01",
		DefaultRegion: "eu-west-1",
		ObservedAt:    firstSeen.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ensure existing User: %v", err)
	}

	if first.ID != "usr_01" || second.ID != first.ID {
		t.Errorf("User IDs = (%q, %q), want stable usr_01", first.ID, second.ID)
	}
	if second.DefaultRegion != "us-east-1" {
		t.Errorf("default region = %q, want preserved us-east-1", second.DefaultRegion)
	}
	if !first.CreatedAt.Equal(firstSeen) || !second.CreatedAt.Equal(firstSeen) {
		t.Errorf("created times = (%s, %s), want %s", first.CreatedAt, second.CreatedAt, firstSeen)
	}
	if !second.UpdatedAt.Equal(firstSeen.Add(time.Minute)) {
		t.Errorf("updated time = %s, want %s", second.UpdatedAt, firstSeen.Add(time.Minute))
	}
	stale, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID:            "usr_stale",
		WorkOSUserID:  "workos_01",
		DefaultRegion: "ap-southeast-1",
		ObservedAt:    firstSeen.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("ensure stale User observation: %v", err)
	}
	if !stale.UpdatedAt.Equal(second.UpdatedAt) {
		t.Errorf("stale observation moved update time to %s, want %s", stale.UpdatedAt, second.UpdatedAt)
	}
}

func TestStoreEnsuresUserProjectionConcurrently(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	firstSeen := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	inputs := []dbstore.EnsureUserInput{
		{ID: "usr_01", WorkOSUserID: "workos_shared", DefaultRegion: "us-east-1", ObservedAt: firstSeen},
		{ID: "usr_02", WorkOSUserID: "workos_shared", DefaultRegion: "eu-west-1", ObservedAt: firstSeen.Add(time.Minute)},
	}
	type result struct {
		user domain.User
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, len(inputs))
	for _, input := range inputs {
		go func() {
			<-start
			user, err := store.EnsureUser(ctx, input)
			results <- result{user: user, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent EnsureUser errors = (%v, %v)", first.err, second.err)
	}
	if first.user.ID != second.user.ID || first.user.DefaultRegion != second.user.DefaultRegion {
		t.Fatalf("concurrent Users diverged: %#v != %#v", first.user, second.user)
	}

	stored, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID:            "usr_stale",
		WorkOSUserID:  "workos_shared",
		DefaultRegion: "ap-southeast-1",
		ObservedAt:    firstSeen.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("read converged User projection: %v", err)
	}
	if stored.ID != first.user.ID || stored.DefaultRegion != first.user.DefaultRegion {
		t.Errorf("stored User identity diverged: %#v", stored)
	}
	if !stored.UpdatedAt.Equal(firstSeen.Add(time.Minute)) {
		t.Errorf("stored update time = %s, want maximum %s", stored.UpdatedAt, firstSeen.Add(time.Minute))
	}
}

func TestStoreRegistersProjectSeedIdempotently(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID: "user-1", WorkOSUserID: "workos-user-1", DefaultRegion: "us-east-1", ObservedAt: createdAt,
	}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	first := newProjectSeed(t, "seed-1", "user-1", digest('a'), createdAt)
	registered, err := store.RegisterProjectSeed(ctx, first, "project-seed-key-1")
	if err != nil {
		t.Fatalf("register Project Seed: %v", err)
	}
	if registered.Snapshot().ID != "seed-1" {
		t.Fatalf("registered Project Seed = %#v", registered.Snapshot())
	}

	replay := newProjectSeed(t, "seed-unused", "user-1", digest('a'), createdAt.Add(time.Minute))
	registered, err = store.RegisterProjectSeed(ctx, replay, "project-seed-key-1")
	if err != nil {
		t.Fatalf("replay Project Seed registration: %v", err)
	}
	if registered.Snapshot().ID != "seed-1" || !registered.Snapshot().CreatedAt.Equal(createdAt) {
		t.Fatalf("replayed Project Seed = %#v", registered.Snapshot())
	}

	conflict := newProjectSeed(t, "seed-conflict", "user-1", digest('f'), createdAt.Add(2*time.Minute))
	if _, err := store.RegisterProjectSeed(ctx, conflict, "project-seed-key-1"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting Project Seed registration error = %v", err)
	}
	var count int
	var repositoryURL, storedDigest string
	if err := pool.QueryRow(ctx, `SELECT count(*), min(repository_url), min(digest) FROM project_seeds`).Scan(&count, &repositoryURL, &storedDigest); err != nil {
		t.Fatalf("read Project Seed rows: %v", err)
	}
	if count != 1 || repositoryURL != "https://github.com/example/project.git" || storedDigest != digest('a') {
		t.Fatalf("Project Seed rows = count:%d repository:%q digest:%q", count, repositoryURL, storedDigest)
	}
}

func TestStoreDeduplicatesProjectSeedContentConcurrently(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID: "user-1", WorkOSUserID: "workos-user-1", DefaultRegion: "us-east-1", ObservedAt: createdAt,
	}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	start := make(chan struct{})
	results := make(chan struct {
		seed domain.ProjectSeed
		err  error
	}, 2)
	for index, key := range []string{"project-seed-key-1", "project-seed-key-2"} {
		candidate := newProjectSeed(t, fmt.Sprintf("seed-%d", index+1), "user-1", digest('a'), createdAt.Add(time.Duration(index)*time.Second))
		go func() {
			<-start
			seed, err := store.RegisterProjectSeed(ctx, candidate, key)
			results <- struct {
				seed domain.ProjectSeed
				err  error
			}{seed: seed, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent registration errors = (%v, %v)", first.err, second.err)
	}
	if first.seed.Snapshot().ID != second.seed.Snapshot().ID {
		t.Fatalf("content address produced Project Seeds %q and %q", first.seed.Snapshot().ID, second.seed.Snapshot().ID)
	}
	var seeds, registrations int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM project_seeds), (SELECT count(*) FROM project_seed_registrations)`).Scan(&seeds, &registrations); err != nil {
		t.Fatalf("count Project Seed registration rows: %v", err)
	}
	if seeds != 1 || registrations != 2 {
		t.Fatalf("registration rows = seeds:%d keys:%d", seeds, registrations)
	}
}

func TestStoreScopesProjectSeedContentAddressesToOwner(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	for _, user := range []dbstore.EnsureUserInput{
		{ID: "user-1", WorkOSUserID: "workos-user-1", DefaultRegion: "us-east-1", ObservedAt: createdAt},
		{ID: "user-2", WorkOSUserID: "workos-user-2", DefaultRegion: "us-east-1", ObservedAt: createdAt},
	} {
		if _, err := store.EnsureUser(ctx, user); err != nil {
			t.Fatalf("ensure User %q: %v", user.ID, err)
		}
	}
	for _, ownerID := range []string{"user-1", "user-2"} {
		seed := newProjectSeed(t, "seed-"+ownerID, ownerID, digest('a'), createdAt)
		if _, err := store.RegisterProjectSeed(ctx, seed, "shared-registration-key"); err != nil {
			t.Fatalf("register Project Seed for %q: %v", ownerID, err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_seeds WHERE digest = $1`, digest('a')).Scan(&count); err != nil {
		t.Fatalf("count owner-scoped Project Seeds: %v", err)
	}
	if count != 2 {
		t.Fatalf("owner-scoped Project Seeds = %d, want 2", count)
	}
}

func newProjectSeed(t *testing.T, id, ownerID, contentDigest string, createdAt time.Time) domain.ProjectSeed {
	t.Helper()
	seed, err := domain.RegisterProjectSeed(domain.ProjectSeedSnapshot{
		ID: id, OwnerUserID: ownerID, RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: contentDigest, ManifestDigest: digest('b'), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("register Project Seed aggregate: %v", err)
	}
	return seed
}

func digest(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return "sha256:" + string(value)
}

func TestStoreReservesEnvironmentCreationIdempotently(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	insertCreationPrerequisites(t, ctx, pool)

	first := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{"name":"workspace"}`), createdAt)
	reserved, err := store.ReserveEnvironmentCreation(ctx, first)
	if err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	if got := reserved.Environment().Snapshot().ID; got != "environment-1" {
		t.Fatalf("reserved Environment ID = %q", got)
	}

	replay := newEnvironmentCreation(t, "environment-unused", "policy-unused", "operation-unused", []byte(`{"name":"workspace"}`), createdAt.Add(time.Minute))
	reserved, err = store.ReserveEnvironmentCreation(ctx, replay)
	if err != nil {
		t.Fatalf("replay Environment creation: %v", err)
	}
	if got := reserved.Environment().Snapshot().ID; got != "environment-1" {
		t.Fatalf("replay Environment ID = %q, want environment-1", got)
	}

	conflict := newEnvironmentCreation(t, "environment-conflict", "policy-conflict", "operation-conflict", []byte(`{"name":"different"}`), createdAt.Add(2*time.Minute))
	if _, err := store.ReserveEnvironmentCreation(ctx, conflict); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v, want ErrIdempotencyConflict", err)
	}

	var environments, operations, policies, assignedSeeds, assignedKeys, outbox int
	for query, destination := range map[string]*int{
		`SELECT count(*) FROM environments`:                                   &environments,
		`SELECT count(*) FROM operations`:                                     &operations,
		`SELECT count(*) FROM auto_stop_policies`:                             &policies,
		`SELECT count(*) FROM project_seeds WHERE environment_id IS NOT NULL`: &assignedSeeds,
		`SELECT count(*) FROM environment_ssh_keys`:                           &assignedKeys,
		`SELECT count(*) FROM workflow_outbox`:                                &outbox,
	} {
		if err := pool.QueryRow(ctx, query).Scan(destination); err != nil {
			t.Fatalf("count projection rows: %v", err)
		}
	}
	if environments != 1 || operations != 1 || policies != 1 || assignedSeeds != 1 || assignedKeys != 1 || outbox != 1 {
		t.Fatalf("projection counts = environments:%d operations:%d policies:%d seeds:%d keys:%d outbox:%d", environments, operations, policies, assignedSeeds, assignedKeys, outbox)
	}
}

func TestStoreTracksEnvironmentCreateInvocationThroughOutbox(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), time.Now().UTC())
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	pending, present, err := store.PendingEnvironmentCreate(ctx, "operation-1")
	if err != nil || !present {
		t.Fatalf("PendingEnvironmentCreate() = %#v, %t, %v", pending, present, err)
	}
	if pending.EnvironmentID != "environment-1" || pending.AvailabilityZone != "us-east-1a" {
		t.Fatalf("pending dispatch = %#v", pending)
	}
	if _, err := store.RecordEnvironmentCreateInvocation(ctx, "operation-1", "invocation-actual", time.Now().UTC()); err != nil {
		t.Fatalf("record Environment create invocation: %v", err)
	}
	if _, present, err := store.PendingEnvironmentCreate(ctx, "operation-1"); err != nil || present {
		t.Fatalf("delivered outbox still pending: present=%t err=%v", present, err)
	}
	reserved, err := store.ReserveEnvironmentCreation(ctx, creation)
	if err != nil {
		t.Fatalf("reload Environment creation: %v", err)
	}
	invocationID := reserved.Operation().Snapshot().RestateInvocationID
	if invocationID == nil || *invocationID != "invocation-actual" {
		t.Fatalf("stored Restate invocation ID = %v", invocationID)
	}
}

func TestStoreRejectsForeignCreationReferencesAtomically(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), time.Now().UTC())
	creation, err := domain.NewEnvironmentCreation(creation.Environment(), creation.Policy(), creation.Operation(), creation.ProjectSeedID(), []string{"foreign-key"})
	if err != nil {
		t.Fatalf("replace creation SSH Key: %v", err)
	}

	if _, err := store.ReserveEnvironmentCreation(ctx, creation); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("reserve with foreign SSH Key error = %v, want ErrReferenceNotOwned", err)
	}
	var environments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM environments`).Scan(&environments); err != nil {
		t.Fatalf("count Environments: %v", err)
	}
	if environments != 0 {
		t.Fatalf("Environment rows after rollback = %d, want 0", environments)
	}
}

func TestStoreCompletesEnvironmentCreationReplaySafely(t *testing.T) {
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

	completedAt := createdAt.Add(time.Minute)
	if _, err := store.CompleteEnvironmentCreation(ctx, "operation-1", completedAt); !errors.Is(err, dbstore.ErrEnvironmentStateRequired) {
		t.Fatalf("complete without Environment State error = %v", err)
	} else {
		requirePermanentRepositoryError(t, err)
	}
	var lifecycle, operationStatus string
	if err := pool.QueryRow(ctx, `
		SELECT environment.lifecycle, operation.status
		FROM environments environment
		JOIN operations operation ON operation.environment_id = environment.id
		WHERE environment.id = 'environment-1' AND operation.id = 'operation-1'`).Scan(&lifecycle, &operationStatus); err != nil {
		t.Fatalf("read creation after rejected completion: %v", err)
	}
	if lifecycle != "creating" || operationStatus != "queued" {
		t.Fatalf("creation changed without State = %s/%s", lifecycle, operationStatus)
	}
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", environmentStateReservation(createdAt.Add(30*time.Second), "volume-1")); err != nil {
		t.Fatalf("inventory Environment State: %v", err)
	}
	completed, err := store.CompleteEnvironmentCreation(ctx, "operation-1", completedAt)
	if err != nil {
		t.Fatalf("complete Environment creation: %v", err)
	}
	if got := completed.Environment().Snapshot(); got.Lifecycle != domain.EnvironmentActive || got.Health != domain.EnvironmentHealthHealthy || got.Version != 3 {
		t.Fatalf("completed Environment = %#v", got)
	}
	if got := completed.Operation().Snapshot(); got.Status != domain.OperationSucceeded || got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
		t.Fatalf("completed Operation = %#v", got)
	}

	replayed, err := store.CompleteEnvironmentCreation(ctx, "operation-1", completedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("replay completion: %v", err)
	}
	if got := replayed.Environment().Snapshot(); got.Version != 3 || !got.UpdatedAt.Equal(completedAt) {
		t.Fatalf("replayed Environment changed = %#v", got)
	}
	if got := replayed.Operation().Snapshot(); got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
		t.Fatalf("replayed Operation changed = %#v", got)
	}
	replayedStateInput := environmentStateReservation(completedAt.Add(time.Minute), "volume-1")
	replayedStateInput.BackendResourceID = "resource-after-completion"
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", replayedStateInput); err != nil {
		t.Fatalf("replay Environment State after completed Operation: %v", err)
	}
}

func openTestStore(t *testing.T, ctx context.Context) *dbstore.Store {
	t.Helper()
	store, _ := openTestStoreAndPool(t, ctx)
	return store
}

func openTestStoreAndPool(t *testing.T, ctx context.Context) (*dbstore.Store, *pgxpool.Pool) {
	t.Helper()
	database, connectionString := openTestDatabase(t, ctx)
	if err := dbstore.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	pool, err := pgxpool.New(ctx, connectionString)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return dbstore.NewStore(pool), pool
}

func insertCreationPrerequisites(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	statements := []string{
		`INSERT INTO users (id, workos_user_id, default_region) VALUES ('user-1', 'workos-1', 'us-east-1')`,
		`INSERT INTO users (id, workos_user_id, default_region) VALUES ('user-2', 'workos-2', 'us-east-1')`,
		`INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-1', 'user-1', 'Default', 'default')`,
		`INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('profile-version-1', 'profile-1', 1, 'sha256:' || repeat('c', 64))`,
		`INSERT INTO project_seeds (id, owner_user_id, repository_url, base_revision, digest, manifest_digest) VALUES ('project-seed-1', 'user-1', 'https://github.com/example/project.git', 'abc123', 'sha256:' || repeat('a', 64), 'sha256:' || repeat('b', 64))`,
		`INSERT INTO ssh_keys (id, owner_user_id, label, algorithm, fingerprint, public_key) VALUES ('ssh-key-1', 'user-1', 'Laptop', 'ssh-ed25519', 'SHA256:key1', 'ssh-ed25519 AAAA')`,
		`INSERT INTO ssh_keys (id, owner_user_id, label, algorithm, fingerprint, public_key) VALUES ('foreign-key', 'user-2', 'Other', 'ssh-ed25519', 'SHA256:key2', 'ssh-ed25519 BBBB')`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("insert creation prerequisite: %v", err)
		}
	}
}

func newEnvironmentCreation(t *testing.T, environmentID, policyID, operationID string, input []byte, createdAt time.Time) domain.EnvironmentCreation {
	t.Helper()
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: environmentID, OwnerUserID: "user-1", Name: "Workspace", Slug: "workspace", Region: "us-east-1",
		AvailabilityZone: "us-east-1a", RuntimePreset: "standard", PinnedProfileVersionID: "profile-version-1",
		AutoStopPolicyID: policyID, CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("reserve domain Environment: %v", err)
	}
	policy, err := domain.NewAutoStopPolicy(policyID, environmentID, domain.AutoStopManual, 0)
	if err != nil {
		t.Fatalf("create domain Auto-stop Policy: %v", err)
	}
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: operationID, EnvironmentID: environmentID, Type: domain.OperationEnvironmentCreate,
		RequestedByUserID: "user-1", IdempotencyKey: "request-key-0001",
		Input: input, CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("queue domain Operation: %v", err)
	}
	creation, err := domain.NewEnvironmentCreation(environment, policy, operation, "project-seed-1", []string{"ssh-key-1"})
	if err != nil {
		t.Fatalf("create domain Environment reservation: %v", err)
	}
	return creation
}
