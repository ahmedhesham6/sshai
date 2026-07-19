//go:build !race

package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/restatedev/sdk-go/ingress"
)

type profileApplyPermanentError struct{ error }

func (profileApplyPermanentError) Transient() bool { return false }

func TestProfileApplyWorkflowPreservesOldLockOnEveryFailure(t *testing.T) {
	for _, test := range []struct {
		name             string
		resolveError     error
		materializeError error
		wantFailure      string
		wantSuccess      bool
	}{
		{name: "happy apply pins tag resolution", wantSuccess: true},
		{name: "resolve failure", resolveError: profileApplyPermanentError{errors.New("tag lookup failed")}, wantFailure: "PROFILE_INCOMPATIBLE"},
		{name: "materialize failure", materializeError: errors.New("managed target drifted"), wantFailure: "PROFILE_CONFLICT"},
		{name: "guest unavailable", materializeError: profileApplyPermanentError{ErrGuestUnavailable}, wantFailure: GuestNotReady},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.July, 19, 15, 0, 0, 0, time.UTC)
			oldLock := profileApplyLock(t, "lock-old", "profile-version-1", "sha256:"+strings.Repeat("a", 64), now.Add(-time.Hour))
			actions := &profileApplyActionsFake{state: ProfileApplyState{
				OwnerUserID: "user-1", Runtime: profileApplyReadyRuntime(t, now), Version: ProfileVersionData{
					ID: "profile-version-2", CapsuleRefs: []domain.CapsuleRef{{Ref: "owner/user-1/capsule:stable", FreshnessPolicy: domain.FreshnessTrack}},
				}, PreviousLock: &oldLock, UpgradePolicy: domain.UpgradeManual,
				Request: ProfileApplyOperationInput{ProfileVersionID: "profile-version-2"},
			}, currentLockID: oldLock.ID}
			resolver := &profileApplyResolverFake{err: test.resolveError, resolution: CapsuleResolution{
				OwnerID: "user-1", Digest: "sha256:" + strings.Repeat("b", 64), Components: []domain.Component{{
					ID: "config:editor", Type: domain.ComponentConfig, MediaType: "application/json", Digest: "sha256:" + strings.Repeat("c", 64),
					SizeBytes: 2, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
				}},
			}}
			guest := &profileApplyGuestFake{err: test.materializeError}
			environment := testfixtures.StartRestate(t, ProfileApplyDefinition(ProfileApplyDependencies{
				Actions: actions, Resolver: resolver, CapsuleApplication: guest, IDs: &profileApplyIDs{value: "lock-new"},
				Now: func() time.Time { return now }, RetryInterval: time.Millisecond, Timeout: time.Second,
			}))
			input := domain.RuntimeOperationDispatch{OperationID: "operation-apply", OperationType: domain.OperationProfileApply, EnvironmentID: "environment-1", RuntimeID: "runtime-1", OwnerUserID: "user-1"}
			if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
				t.Fatalf("send Profile apply: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			output, err := ingress.WorkflowHandle[ProfileApplyOutput](environment.Ingress(), ProfileApplyService, input.OperationID).Attach(ctx)
			if test.wantSuccess {
				if err != nil || output.CapsuleLockID != "lock-new" || actions.currentLockID != "lock-new" || actions.completeCalls != 1 {
					t.Fatalf("happy apply = output:%#v error:%v lock:%q complete:%d", output, err, actions.currentLockID, actions.completeCalls)
				}
				if resolver.lastRef.Ref != "owner/user-1/capsule:stable" || resolver.lastRef.FreshnessPolicy != domain.FreshnessTrack {
					t.Fatalf("tag resolution input = %#v", resolver.lastRef)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), test.wantFailure) || actions.failureCode != test.wantFailure {
					t.Fatalf("failure = error:%v code:%q, want %q", err, actions.failureCode, test.wantFailure)
				}
				if actions.currentLockID != "lock-old" || actions.completeCalls != 0 || actions.state.Runtime.Status != domain.RuntimeReady {
					t.Fatalf("failure changed authoritative state = lock:%q complete:%d Runtime:%q", actions.currentLockID, actions.completeCalls, actions.state.Runtime.Status)
				}
			}
		})
	}
}

type profileApplyActionsFake struct {
	state         ProfileApplyState
	currentLockID string
	completeCalls int
	failureCode   string
}

func (fake *profileApplyActionsFake) LoadProfileApplyOperation(context.Context, domain.RuntimeOperationDispatch, string, time.Time) (ProfileApplyState, error) {
	return fake.state, nil
}

func (fake *profileApplyActionsFake) CompleteProfileApply(_ context.Context, _ string, previous *string, state EnvironmentCapsuleState, _ time.Time) error {
	if previous == nil || *previous != fake.currentLockID {
		return errors.New("old Lock changed")
	}
	fake.completeCalls++
	fake.currentLockID = state.CapsuleLock.ID
	return nil
}

func (fake *profileApplyActionsFake) RecordProfileApplyFailure(_ context.Context, _ string, code, _ string, _ time.Time) error {
	fake.failureCode = code
	return nil
}

type profileApplyResolverFake struct {
	resolution CapsuleResolution
	err        error
	lastRef    domain.CapsuleRef
}

func (fake *profileApplyResolverFake) Resolve(_ context.Context, _ string, ref domain.CapsuleRef) (CapsuleResolution, error) {
	if at := strings.LastIndex(ref.Ref, "@sha256:"); at >= 0 {
		return CapsuleResolution{OwnerID: "user-1", Digest: ref.Ref[at+1:]}, fake.err
	}
	fake.lastRef = ref
	return fake.resolution, fake.err
}

type profileApplyIDs struct{ value string }

func (ids *profileApplyIDs) NewID() string { return ids.value }

type profileApplyGuestFake struct{ err error }

func (fake *profileApplyGuestFake) EnsureEnvironmentCapsuleMaterialized(_ context.Context, request EnvironmentCapsuleMaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	lock := request.State.CapsuleLock
	component := lock.ResolvedComponents["config:editor"]
	return []profile.ProfileMaterializationResult{{
		ID: component.ID, LockID: lock.ID, LockDigest: lock.Digest, CapsuleDigest: component.CapsuleDigest,
		ComponentID: component.ID, ComponentDigest: component.ComponentDigest, Adapter: "claude", AdapterID: "claude",
		AdapterVersion: "1", TargetAgentVersion: "1", Scope: component.Scope, EffectiveCacheKey: "cache-key",
		Mode: profile.MaterializationManaged, Root: profile.MaterializationHome, Target: ".claude/settings.json", Selector: "$",
		DesiredDigest: component.ComponentDigest, LastAppliedDigest: component.ComponentDigest, ObservedDigest: component.ComponentDigest,
		Operation: profile.OperationCreate,
	}}, nil
}

func profileApplyReadyRuntime(t *testing.T, now time.Time) domain.RuntimeSnapshot {
	t.Helper()
	providerID, address, bootID := "i-runtime-1", "10.0.0.8", "boot-1"
	startedAt := now.Add(-time.Hour)
	runtime, err := domain.RestoreRuntime(domain.RuntimeSnapshot{
		ID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1, Status: domain.RuntimeReady,
		RuntimePreset: "standard", Region: "eu-central-1", AvailabilityZone: "eu-central-1a", ImageVersion: "image-1",
		ProviderInstanceRef: &providerID, PrivateAddress: &address, BootID: &bootID, StartedAt: &startedAt,
		CreatedAt: startedAt, UpdatedAt: now, Version: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime.Snapshot()
}

func profileApplyLock(t *testing.T, id, versionID, capsuleDigest string, at time.Time) domain.CapsuleLockSnapshot {
	t.Helper()
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: id, EnvironmentID: "environment-1", ProfileVersionID: versionID,
		ProjectCapsuleDigest: "sha256:" + strings.Repeat("e", 64),
		Capsules:             []domain.LockedCapsule{{Ref: "owner/user-1/capsule:stable", Digest: capsuleDigest}}, CreatedAt: at.UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return lock.Snapshot()
}
