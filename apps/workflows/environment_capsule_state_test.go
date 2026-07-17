package workflows_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/apps/workflows"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestEnvironmentCreationCapsuleActionsPersistsLockPinAndApplyResults(t *testing.T) {
	at := time.Date(2026, time.July, 17, 16, 0, 0, 0, time.UTC)
	digest := workflowTestDigest('a')
	snapshot := domain.CapsuleLockSnapshot{
		ID: "lock-1", EnvironmentID: "environment-1", ProfileVersionID: "version-1", ProjectCapsuleDigest: digest,
		Capsules: []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + digest, Digest: digest}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {
				ID: "config:editor", Type: domain.ComponentConfig, CapsuleDigest: digest, ComponentDigest: digest,
				Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
			},
		},
		CreatedAt: at,
	}
	lock, err := domain.CreateCapsuleLock(snapshot)
	if err != nil {
		t.Fatalf("create Capsule Lock: %v", err)
	}
	snapshot = lock.Snapshot()
	state := workflows.EnvironmentCapsuleState{
		CapsuleLock: snapshot, UpgradePolicy: domain.UpgradeNotify,
		Materializations: []guest.InstalledMaterialization{{
			ID: "editor", LockID: snapshot.ID, LockDigest: snapshot.Digest, CapsuleDigest: digest,
			ComponentID: "config:editor", ComponentDigest: digest, AdapterID: "file", AdapterVersion: "v1",
			TargetAgentVersion: "agent-1", Scope: domain.ScopeUser, NonSecretOverridesDigest: workflowTestDigest('b'),
			SecretVersionIdentifiers: []string{"secret-v1"}, EffectiveCacheKey: workflowTestDigest('c'),
			Mode: "managed", Root: "home", Target: ".config/editor.json", Selector: "$", Directory: true,
			FilePaths: []string{"editor.json"}, LastAppliedDigest: workflowTestDigest('d'), ObservedDigest: workflowTestDigest('e'),
			CredentialRequirementDigest: workflowTestDigest('f'),
		}},
	}
	resolver := &pinnedProfileResolverFake{state: state}
	repository := &capsuleStateRepositoryFake{}
	actions := workflows.NewEnvironmentCreationCapsuleActions(resolver, repository)

	resolved, err := actions.ResolvePinnedProfileVersion(t.Context(), "operation-1", at)
	if err != nil {
		t.Fatalf("ResolvePinnedProfileVersion(): %v", err)
	}
	if err := actions.PersistEnvironmentCapsuleState(t.Context(), "operation-1", resolved); err != nil {
		t.Fatalf("PersistEnvironmentCapsuleState(): %v", err)
	}

	got := repository.input
	if got.EnvironmentID != snapshot.EnvironmentID || got.CapsuleLock.Snapshot().Digest != snapshot.Digest || got.UpgradePolicy != domain.UpgradeNotify {
		t.Fatalf("persisted Capsule state = %#v", got)
	}
	if len(got.Materializations) != 1 {
		t.Fatalf("persisted Materializations = %#v", got.Materializations)
	}
	record := got.Materializations[0]
	if record.ComponentID != "config:editor" || record.ComponentType != domain.ComponentConfig || record.TrustClass != domain.TrustDeclarative ||
		record.EffectiveCacheKey != workflowTestDigest('c') || record.SecretVersionIdentifiers[0] != "secret-v1" ||
		!record.CreatedAt.Equal(at) || !record.UpdatedAt.Equal(at) {
		t.Fatalf("persisted Materialization = %#v", record)
	}
}

type pinnedProfileResolverFake struct {
	state workflows.EnvironmentCapsuleState
}

func (fake *pinnedProfileResolverFake) ResolvePinnedProfileVersion(context.Context, string, time.Time) (workflows.EnvironmentCapsuleState, error) {
	return fake.state, nil
}

type capsuleStateRepositoryFake struct {
	input dbstore.EnvironmentCapsuleStateInput
}

func (fake *capsuleStateRepositoryFake) PersistEnvironmentCapsuleState(_ context.Context, input dbstore.EnvironmentCapsuleStateInput) error {
	fake.input = input
	return nil
}

func workflowTestDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}
