package guest_test

import (
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestProposeDriftAdoptionIdentifiesOwningCapsuleWithoutMutatingState(t *testing.T) {
	lock := domain.CapsuleLockSnapshot{
		ProjectCapsuleDigest: sha256GuestDigest('f'),
		Capsules:             []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + sha256GuestDigest('a'), Digest: sha256GuestDigest('a')}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {ID: "config:editor", CapsuleDigest: sha256GuestDigest('a'), ComponentDigest: sha256GuestDigest('b'), Type: domain.ComponentConfig, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative},
		},
	}
	installed := guest.InstalledMaterialization{ComponentID: "config:editor", Mode: guest.MaterializationManaged, CapsuleDigest: sha256GuestDigest('a'), LastAppliedDigest: sha256GuestDigest('b')}
	observed := sha256GuestDigest('c')
	proposal, err := guest.ProposeDriftAdoption(lock, "user-1", installed, observed, "theme changed remotely")
	if err != nil {
		t.Fatalf("ProposeDriftAdoption(): %v", err)
	}
	if proposal.ComponentID != "config:editor" || proposal.OwningCapsuleRef != lock.Capsules[0].Ref || proposal.ObservedDigest != observed || proposal.DiffSummary != "theme changed remotely" {
		t.Fatalf("drift adoption proposal = %#v", proposal)
	}
	if proposal.RequiresReview || proposal.ConsentRequired == false {
		t.Fatalf("declarative adoption proposal = %#v, want consent without executable review", proposal)
	}
	if installed.ObservedDigest != "" {
		t.Fatal("proposal generation mutated installed state")
	}
}

func TestDriftAdoptionAcceptanceRequiresExplicitConsentAndExecutableReview(t *testing.T) {
	lock := domain.CapsuleLockSnapshot{
		Capsules: []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + sha256GuestDigest('a'), Digest: sha256GuestDigest('a')}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"hook:format": {ID: "hook:format", CapsuleDigest: sha256GuestDigest('a'), ComponentDigest: sha256GuestDigest('b'), Type: domain.ComponentHook, Scope: domain.ScopeUser, TrustClass: domain.TrustExecutable},
		},
	}
	proposal, err := guest.ProposeDriftAdoption(lock, "user-1", guest.InstalledMaterialization{ComponentID: "hook:format", Mode: guest.MaterializationManaged, CapsuleDigest: sha256GuestDigest('a'), LastAppliedDigest: sha256GuestDigest('b')}, sha256GuestDigest('c'), "hook changed")
	if err != nil {
		t.Fatalf("ProposeDriftAdoption(): %v", err)
	}
	if !proposal.RequiresReview {
		t.Fatalf("executable proposal = %#v, want review", proposal)
	}
	if _, err := guest.AcceptDriftAdoption(lock, "user-1", proposal, guest.DriftAdoptionConsent{}); err == nil {
		t.Fatal("unconsented drift adoption succeeded")
	}
	if _, err := guest.AcceptDriftAdoption(lock, "user-1", proposal, guest.DriftAdoptionConsent{Accepted: true}); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("executable adoption without review error = %v", err)
	}
	accepted, err := guest.AcceptDriftAdoption(lock, "user-1", proposal, guest.DriftAdoptionConsent{Accepted: true, ExecutableReviewApproved: true})
	if err != nil {
		t.Fatalf("AcceptDriftAdoption(): %v", err)
	}
	if !accepted.Accepted || accepted.Proposal.ComponentID != "hook:format" {
		t.Fatalf("accepted adoption = %#v", accepted)
	}
}

func TestDriftAdoptionAcceptanceIgnoresForgedRequiresReviewFalse(t *testing.T) {
	lock := domain.CapsuleLockSnapshot{
		Capsules: []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + sha256GuestDigest('a'), Digest: sha256GuestDigest('a')}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"command:run": {ID: "command:run", CapsuleDigest: sha256GuestDigest('a'), ComponentDigest: sha256GuestDigest('b'), Type: domain.ComponentCommand, Scope: domain.ScopeUser, TrustClass: domain.TrustExecutable},
		},
	}
	proposal, err := guest.ProposeDriftAdoption(lock, "user-1", guest.InstalledMaterialization{ComponentID: "command:run", Mode: guest.MaterializationManaged, CapsuleDigest: sha256GuestDigest('a'), LastAppliedDigest: sha256GuestDigest('b')}, sha256GuestDigest('c'), "command changed")
	if err != nil {
		t.Fatalf("ProposeDriftAdoption(): %v", err)
	}
	proposal.RequiresReview = false
	if _, err := guest.AcceptDriftAdoption(lock, "user-1", proposal, guest.DriftAdoptionConsent{Accepted: true}); err == nil || !strings.Contains(err.Error(), "review") {
		t.Fatalf("forged executable proposal acceptance error = %v, want review rejection", err)
	}
}

func TestDriftAdoptionRejectsForeignOwningCapsuleReference(t *testing.T) {
	lock := domain.CapsuleLockSnapshot{
		Capsules: []domain.LockedCapsule{{Ref: "owner/user-1/capsule@" + sha256GuestDigest('a'), Digest: sha256GuestDigest('a')}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {ID: "config:editor", CapsuleDigest: sha256GuestDigest('a'), ComponentDigest: sha256GuestDigest('b'), Type: domain.ComponentConfig, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative},
		},
	}
	installed := guest.InstalledMaterialization{ComponentID: "config:editor", Mode: guest.MaterializationManaged, CapsuleDigest: sha256GuestDigest('a'), LastAppliedDigest: sha256GuestDigest('b')}
	if _, err := guest.ProposeDriftAdoption(lock, "user-2", installed, sha256GuestDigest('c'), "foreign owner"); err == nil {
		t.Fatal("foreign owner drift adoption proposal succeeded")
	}
}

func TestReconcileCapsuleLockMaterializationsReportsLockComponentDivergence(t *testing.T) {
	lock := domain.CapsuleLockSnapshot{
		Digest: sha256GuestDigest('a'),
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:kept":    {ID: "config:kept", CapsuleDigest: sha256GuestDigest('b'), ComponentDigest: sha256GuestDigest('c')},
			"config:missing": {ID: "config:missing", CapsuleDigest: sha256GuestDigest('b'), ComponentDigest: sha256GuestDigest('d')},
		},
	}
	installed := []guest.InstalledMaterialization{
		{ComponentID: "config:kept", LockDigest: lock.Digest, CapsuleDigest: sha256GuestDigest('b'), ComponentDigest: sha256GuestDigest('z')},
		{ComponentID: "config:extra", LockDigest: lock.Digest, CapsuleDigest: sha256GuestDigest('b'), ComponentDigest: sha256GuestDigest('e')},
	}
	divergences := guest.ReconcileCapsuleLockMaterializations(lock, installed)
	if len(divergences) != 3 {
		t.Fatalf("lock/materialization divergences = %#v, want changed/missing/extra", divergences)
	}
}

func sha256GuestDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}
