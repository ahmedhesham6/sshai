package workflows_test

import (
	"context"
	"testing"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestConsumeDriftAdoptionRequiresConsentBeforeWorkflowMutation(t *testing.T) {
	lock := domain.CapsuleLockSnapshot{
		Capsules: []domain.LockedCapsule{{Ref: "owner/user-1/capsule@sha256:" + repeatedWorkflowDigest('a'), Digest: "sha256:" + repeatedWorkflowDigest('a')}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {ID: "config:editor", CapsuleDigest: "sha256:" + repeatedWorkflowDigest('a'), ComponentDigest: "sha256:" + repeatedWorkflowDigest('b'), Type: domain.ComponentConfig, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative},
		},
	}
	proposal, err := guest.ProposeDriftAdoption(lock, "user-1", guest.InstalledMaterialization{ComponentID: "config:editor", Mode: guest.MaterializationManaged, CapsuleDigest: "sha256:" + repeatedWorkflowDigest('a'), LastAppliedDigest: "sha256:" + repeatedWorkflowDigest('b')}, "sha256:"+repeatedWorkflowDigest('c'), "theme changed")
	if err != nil {
		t.Fatalf("propose drift adoption: %v", err)
	}
	actions := &driftAdoptionActionsFake{}
	if err := workflows.ConsumeDriftAdoption(t.Context(), "user-1", lock, proposal, guest.DriftAdoptionConsent{}, actions); err == nil {
		t.Fatal("unconsented drift adoption succeeded")
	}
	if actions.acceptCalls != 0 {
		t.Fatalf("unconsented workflow mutation calls = %d, want zero", actions.acceptCalls)
	}
	if err := workflows.ConsumeDriftAdoption(t.Context(), "user-1", lock, proposal, guest.DriftAdoptionConsent{Accepted: true}, actions); err != nil {
		t.Fatalf("consume consented drift adoption: %v", err)
	}
	if actions.acceptCalls != 1 {
		t.Fatalf("consented workflow mutation calls = %d, want one", actions.acceptCalls)
	}
}

type driftAdoptionActionsFake struct {
	acceptCalls int
}

func (actions *driftAdoptionActionsFake) RecordDriftAdoptionProposal(context.Context, guest.DriftAdoptionProposal) error {
	return nil
}

func (actions *driftAdoptionActionsFake) AcceptDriftAdoption(context.Context, guest.AcceptedDriftAdoption) error {
	actions.acceptCalls++
	return nil
}

func repeatedWorkflowDigest(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return string(value)
}
