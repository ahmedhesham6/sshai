package profile_test

import (
	"testing"

	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestManagedMaterializationPlansSafeChanges(t *testing.T) {
	missing := profile.AbsentDigest()
	old := presentDigest(t, "sha256:old")
	updated := presentDigest(t, "sha256:updated")

	tests := []struct {
		name     string
		desired  profile.DigestState
		applied  profile.DigestState
		observed profile.DigestState
		want     profile.PlanOperation
	}{
		{name: "new target", desired: updated, applied: missing, observed: missing, want: profile.OperationCreate},
		{name: "unchanged target", desired: old, applied: old, observed: old, want: profile.OperationSkip},
		{name: "safe update", desired: updated, applied: old, observed: old, want: profile.OperationUpdate},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := profile.NewManagedMaterialization(test.desired, test.applied, test.observed)
			operation, err := profile.PlanMaterialization(snapshot, profile.IntentReconcile)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if operation != test.want {
				t.Fatalf("operation = %q, want %q", operation, test.want)
			}
		})
	}
}

func TestManagedMaterializationPreservesRemoteChanges(t *testing.T) {
	missing := profile.AbsentDigest()
	applied := presentDigest(t, "sha256:applied")
	remote := presentDigest(t, "sha256:remote")
	desired := presentDigest(t, "sha256:desired")

	tests := []struct {
		name     string
		desired  profile.DigestState
		applied  profile.DigestState
		observed profile.DigestState
		want     profile.PlanOperation
	}{
		{name: "remote drift", desired: applied, applied: applied, observed: remote, want: profile.OperationDrift},
		{name: "remote deletion", desired: applied, applied: applied, observed: missing, want: profile.OperationDrift},
		{name: "concurrent changes", desired: desired, applied: applied, observed: remote, want: profile.OperationConflict},
		{name: "concurrent change and deletion", desired: desired, applied: applied, observed: missing, want: profile.OperationConflict},
		{name: "preexisting target", desired: desired, applied: missing, observed: remote, want: profile.OperationConflict},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := profile.NewManagedMaterialization(test.desired, test.applied, test.observed)
			operation, err := profile.PlanMaterialization(snapshot, profile.IntentReconcile)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if operation != test.want {
				t.Fatalf("operation = %q, want %q", operation, test.want)
			}
		})
	}
}

func TestSeededMaterializationTransfersOwnershipAfterCreation(t *testing.T) {
	missing := profile.AbsentDigest()
	seed := presentDigest(t, "sha256:seed")
	changedSource := presentDigest(t, "sha256:changed-source")
	changedRemote := presentDigest(t, "sha256:changed-remote")

	tests := []struct {
		name     string
		desired  profile.DigestState
		applied  profile.DigestState
		observed profile.DigestState
		want     profile.PlanOperation
	}{
		{name: "create once", desired: seed, applied: missing, observed: missing, want: profile.OperationCreate},
		{name: "protect preexisting target", desired: seed, applied: missing, observed: changedRemote, want: profile.OperationConflict},
		{name: "preserve environment-owned change", desired: changedSource, applied: seed, observed: changedRemote, want: profile.OperationSkip},
		{name: "preserve environment-owned deletion", desired: changedSource, applied: seed, observed: missing, want: profile.OperationSkip},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := profile.NewSeededMaterialization(test.desired, test.applied, test.observed)
			operation, err := profile.PlanMaterialization(snapshot, profile.IntentReconcile)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if operation != test.want {
				t.Fatalf("operation = %q, want %q", operation, test.want)
			}
		})
	}
}

func TestReferencedMaterializationRequiresEnvironmentInput(t *testing.T) {
	requirement := presentDigest(t, "sha256:credential-requirement")

	tests := []struct {
		name    string
		binding profile.RequirementState
		want    profile.PlanOperation
	}{
		{name: "unbound requirement", binding: profile.RequirementNeedsInput, want: profile.OperationRequiresInput},
		{name: "bound requirement", binding: profile.RequirementBound, want: profile.OperationSkip},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := profile.NewReferencedMaterialization(requirement, profile.AbsentDigest(), test.binding)
			if err != nil {
				t.Fatalf("record referenced materialization: %v", err)
			}
			operation, err := profile.PlanMaterialization(snapshot, profile.IntentReconcile)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if operation != test.want {
				t.Fatalf("operation = %q, want %q", operation, test.want)
			}
		})
	}
}

func TestRemovedSourceIsOrphanedInEveryModeUntilExplicitlyPruned(t *testing.T) {
	missing := profile.AbsentDigest()
	applied := presentDigest(t, "sha256:applied")
	referenced, err := profile.NewReferencedMaterialization(missing, applied, profile.RequirementBound)
	if err != nil {
		t.Fatalf("record removed referenced materialization: %v", err)
	}

	tests := []struct {
		name     string
		snapshot profile.MaterializationSnapshot
	}{
		{name: "managed", snapshot: profile.NewManagedMaterialization(missing, applied, applied)},
		{name: "seeded", snapshot: profile.NewSeededMaterialization(missing, applied, applied)},
		{name: "referenced", snapshot: referenced},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation, err := profile.PlanMaterialization(test.snapshot, profile.IntentReconcile)
			if err != nil {
				t.Fatalf("plan removed source: %v", err)
			}
			if operation != profile.OperationOrphan {
				t.Fatalf("reconcile operation = %q, want %q", operation, profile.OperationOrphan)
			}

			operation, err = profile.PlanMaterialization(test.snapshot, profile.IntentPrune)
			if err != nil {
				t.Fatalf("plan explicit prune: %v", err)
			}
			if operation != profile.OperationRemove {
				t.Fatalf("prune operation = %q, want %q", operation, profile.OperationRemove)
			}
		})
	}
}

func TestPruneNeverRemovesAStillSourcedOrAlreadyAbsentTarget(t *testing.T) {
	missing := profile.AbsentDigest()
	source := presentDigest(t, "sha256:source")

	if _, err := profile.PlanMaterialization(
		profile.NewManagedMaterialization(source, source, source),
		profile.IntentPrune,
	); err == nil {
		t.Fatal("expected prune of a still-sourced materialization to be rejected")
	}

	operation, err := profile.PlanMaterialization(
		profile.NewManagedMaterialization(missing, source, missing),
		profile.IntentPrune,
	)
	if err != nil {
		t.Fatalf("plan prune of absent target: %v", err)
	}
	if operation != profile.OperationSkip {
		t.Fatalf("operation = %q, want %q", operation, profile.OperationSkip)
	}
}

func TestMaterializationPlanningRejectsInvalidState(t *testing.T) {
	if _, err := profile.PresentDigest(""); err == nil {
		t.Fatal("expected an empty present digest to be rejected")
	}
	if _, err := profile.PlanMaterialization(profile.MaterializationSnapshot{}, profile.IntentReconcile); err == nil {
		t.Fatal("expected an uninitialized snapshot to be rejected")
	}
	if _, err := profile.PlanMaterialization(
		profile.NewManagedMaterialization(
			presentDigest(t, "sha256:desired"),
			profile.AbsentDigest(),
			profile.AbsentDigest(),
		),
		profile.PlanIntent("unknown"),
	); err == nil {
		t.Fatal("expected an unknown plan intent to be rejected")
	}
	if _, err := profile.NewReferencedMaterialization(
		presentDigest(t, "sha256:requirement"),
		profile.AbsentDigest(),
		profile.RequirementState("unknown"),
	); err == nil {
		t.Fatal("expected an unknown requirement state to be rejected")
	}
}

func presentDigest(t *testing.T, digest string) profile.DigestState {
	t.Helper()
	state, err := profile.PresentDigest(digest)
	if err != nil {
		t.Fatalf("present digest: %v", err)
	}
	return state
}
