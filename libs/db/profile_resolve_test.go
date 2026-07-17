package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestLoadProfileResolveStateReturnsPinnedApprovalsAndManagedTargets(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 16, 0, 0, 0, time.UTC)
	seedEnvironmentMaterializationPrerequisites(t, ctx, pool)

	capsuleDigest := testDigest('a')
	lock := persistEnvironmentMaterializationLockFixture(
		t, ctx, store, "lock-state-1", "environment-1", "version-1", capsuleDigest, now,
		"config:editor",
	)
	if _, err := store.UpsertEnvironmentPin(ctx, dbstore.EnvironmentPinInput{
		EnvironmentID: "environment-1", CapsuleLockID: stringPointer(lock.Snapshot().ID), UpgradePolicy: domain.UpgradeNotify,
	}); err != nil {
		t.Fatalf("UpsertEnvironmentPin(): %v", err)
	}
	materialization := environmentMaterializationFixture("config:editor", lock.Snapshot().ID, now)
	materialization.LockDigest = lock.Snapshot().Digest
	materialization.CapsuleDigest = capsuleDigest
	if err := store.UpsertEnvironmentMaterializations(ctx, []dbstore.EnvironmentMaterialization{materialization}); err != nil {
		t.Fatalf("UpsertEnvironmentMaterializations(): %v", err)
	}

	state, err := store.LoadProfileResolveState(ctx, "environment-1")
	if err != nil {
		t.Fatalf("LoadProfileResolveState(): %v", err)
	}
	if state.PersistedUpgradePolicy == nil || *state.PersistedUpgradePolicy != domain.UpgradeNotify {
		t.Fatalf("persisted upgrade policy = %#v, want notify", state.PersistedUpgradePolicy)
	}
	wantRef := lock.Snapshot().Capsules[0].Ref
	if len(state.LastApprovedCapsuleDigests) != 1 || state.LastApprovedCapsuleDigests[wantRef] != capsuleDigest {
		t.Fatalf("last approved Capsule digests = %#v, want {%q: %q}", state.LastApprovedCapsuleDigests, wantRef, capsuleDigest)
	}
	if len(state.ManagedTargets) != 1 {
		t.Fatalf("managed targets = %#v, want one entry", state.ManagedTargets)
	}
	target := state.ManagedTargets[0]
	if target.ComponentID != "config:editor" || target.DesiredDigest != materialization.ComponentDigest ||
		target.LastAppliedDigest != materialization.LastAppliedDigest || target.ObservedDigest != materialization.ObservedDigest {
		t.Fatalf("managed target = %#v, want fields sourced from Environment Materialization", target)
	}
}

func TestLoadProfileResolveStateReturnsEmptyApprovalsForUnpinnedEnvironment(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	seedEnvironmentMaterializationPrerequisites(t, ctx, pool)

	state, err := store.LoadProfileResolveState(ctx, "environment-1")
	if err != nil {
		t.Fatalf("LoadProfileResolveState(): %v", err)
	}
	if state.PersistedUpgradePolicy == nil || *state.PersistedUpgradePolicy != domain.UpgradeManual {
		t.Fatalf("persisted upgrade policy = %#v, want default manual", state.PersistedUpgradePolicy)
	}
	if len(state.LastApprovedCapsuleDigests) != 0 {
		t.Fatalf("last approved Capsule digests = %#v, want none for an unpinned Environment", state.LastApprovedCapsuleDigests)
	}
	if len(state.ManagedTargets) != 0 {
		t.Fatalf("managed targets = %#v, want none for an unpinned Environment", state.ManagedTargets)
	}
}

func TestLoadProfileResolveStateRejectsUnknownEnvironment(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)

	if _, err := store.LoadProfileResolveState(ctx, "environment-missing"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("LoadProfileResolveState() error = %v, want ErrReferenceNotOwned", err)
	}
}

func stringPointer(value string) *string {
	return &value
}
