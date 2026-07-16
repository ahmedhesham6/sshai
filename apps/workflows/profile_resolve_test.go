package workflows_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/restatedev/sdk-go/ingress"
)

func TestProfileResolveWorkflowPersistsOneImmutableLockAndReplaysIt(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	version := profileVersionFixture(t, now)
	actions := &profileResolveActionsFake{version: version}
	resolver := profileResolverFake{resolution: workflows.CapsuleResolution{
		OwnerID: "user-1",
		Digest:  "sha256:" + strings.Repeat("a", 64),
		Components: []domain.Component{{
			ID: "config:editor", Type: domain.ComponentConfig, MediaType: "application/json",
			Digest: "sha256:" + strings.Repeat("c", 64), SizeBytes: 7, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
		}},
	}}
	ids := &resolveIDs{values: []string{"lock-1"}}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, &resolver, ids, func() time.Time { return now }))
	client := workflows.NewProfileResolveClient(environment.Ingress())
	input := workflows.ProfileResolveInput{OperationID: "operation-1", EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1"}

	if err := client.SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit Profile resolve workflow: %v", err)
	}
	handle := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID)
	first, err := handle.Attach(t.Context())
	if err != nil {
		t.Fatalf("await Profile resolve workflow: %v", err)
	}
	second, err := handle.Attach(t.Context())
	if err != nil {
		t.Fatalf("replay Profile resolve workflow: %v", err)
	}
	if first != second || first.LockID != "lock-1" || first.Digest == "" {
		t.Fatalf("Profile resolve outputs = %#v and %#v, want same lock", first, second)
	}
	if actions.persistCalls != 1 || actions.completeCalls != 1 || actions.lock.Snapshot().Digest != first.Digest {
		t.Fatalf("Profile resolve action calls = persist:%d complete:%d lock:%#v", actions.persistCalls, actions.completeCalls, actions.lock.Snapshot())
	}
}

func TestProfileResolveWorkflowResolvesDigestRefWithoutTagLookup(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	version := profileVersionFixture(t, now)
	version.CapsuleRefs[0].Ref = "owner/user-1/capsule@sha256:" + strings.Repeat("d", 64)
	actions := &profileResolveActionsFake{version: version}
	resolver := profileResolverFake{resolution: workflows.CapsuleResolution{OwnerID: "user-1", Digest: "sha256:" + strings.Repeat("d", 64)}}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, &resolver, &resolveIDs{values: []string{"lock-2"}}, func() time.Time { return now }))
	input := workflows.ProfileResolveInput{OperationID: "operation-2", EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1"}
	if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit digest Profile resolve workflow: %v", err)
	}
	if _, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context()); err != nil {
		t.Fatalf("await digest Profile resolve workflow: %v", err)
	}
	if resolver.lastRef.Ref != version.CapsuleRefs[0].Ref || resolver.calls != 1 {
		t.Fatalf("digest resolver input = %#v calls:%d", resolver.lastRef, resolver.calls)
	}
}

func TestProfileResolveWorkflowRejectsResolutionFromForeignOwner(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	version := profileVersionFixture(t, now)
	actions := &profileResolveActionsFake{version: version}
	resolver := profileResolverFake{resolution: workflows.CapsuleResolution{
		OwnerID: "user-2",
		Digest:  "sha256:" + strings.Repeat("b", 64),
	}}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, &resolver, &resolveIDs{values: []string{"lock-foreign"}}, func() time.Time { return now }))
	input := workflows.ProfileResolveInput{OperationID: "operation-foreign", EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1"}
	if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit foreign-owner Profile resolve workflow: %v", err)
	}
	_, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context())
	if err == nil {
		t.Fatal("foreign-owner Profile resolve succeeded, want rejection")
	}
	if actions.persistCalls != 0 {
		t.Fatalf("foreign-owner resolution persisted %d Capsule Locks", actions.persistCalls)
	}
	if resolver.lastOwnerID != "user-1" {
		t.Fatalf("resolver owner ID = %q, want user-1", resolver.lastOwnerID)
	}
}

func profileVersionFixture(t *testing.T, now time.Time) domain.ProfileVersionData {
	t.Helper()
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: "profile-1", OwnerUserID: "user-1", Name: "Personal", Slug: "personal", CreatedAt: now})
	if err != nil {
		t.Fatalf("create Profile fixture: %v", err)
	}
	version, err := profile.PublishVersion(nil, nil, domain.ProfileVersionPublication{
		ID: "version-1", Digest: domain.ComputeProfileVersionDigest([]domain.CapsuleRef{{Ref: "owner/user-1/capsule@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FreshnessPolicy: domain.FreshnessPin}}),
		CapsuleRefs: []domain.CapsuleRef{{Ref: "owner/user-1/capsule@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FreshnessPolicy: domain.FreshnessPin}}, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("publish Profile fixture: %v", err)
	}
	snapshot := version.Snapshot()
	return domain.ProfileVersionData{ID: snapshot.ID, CapsuleRefs: snapshot.CapsuleRefs}
}

type profileResolveActionsFake struct {
	mu            sync.Mutex
	version       domain.ProfileVersionData
	state         workflows.ProfileResolveState
	lock          domain.CapsuleLock
	persistCalls  int
	completeCalls int
}

func (actions *profileResolveActionsFake) RecordProfileResolveInvocation(context.Context, string, string, string, string, *string, time.Time) error {
	return nil
}

func (actions *profileResolveActionsFake) LoadProfileVersion(context.Context, string, string) (domain.ProfileVersionData, error) {
	return actions.version, nil
}

func (actions *profileResolveActionsFake) LoadProfileResolveState(context.Context, string) (workflows.ProfileResolveState, error) {
	return actions.state, nil
}

func (actions *profileResolveActionsFake) PersistCapsuleLock(_ context.Context, snapshot domain.CapsuleLockSnapshot) (domain.CapsuleLockSnapshot, error) {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	actions.persistCalls++
	if actions.lock.Snapshot().ID != "" {
		return actions.lock.Snapshot(), nil
	}
	lock, err := domain.CreateCapsuleLock(snapshot)
	if err != nil {
		return domain.CapsuleLockSnapshot{}, err
	}
	actions.lock = lock
	return lock.Snapshot(), nil
}

func (actions *profileResolveActionsFake) CompleteProfileResolve(context.Context, string, time.Time) error {
	actions.completeCalls++
	return nil
}

type profileResolverFake struct {
	resolution  workflows.CapsuleResolution
	lastRef     domain.CapsuleRef
	lastOwnerID string
	calls       int
}

func (resolver *profileResolverFake) Resolve(_ context.Context, ownerID string, ref domain.CapsuleRef) (workflows.CapsuleResolution, error) {
	resolver.calls++
	resolver.lastOwnerID = ownerID
	resolver.lastRef = ref
	return resolver.resolution, nil
}

type resolveIDs struct {
	mu     sync.Mutex
	values []string
}

func (ids *resolveIDs) NewID() string {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	value := ids.values[0]
	ids.values = ids.values[1:]
	return value
}
