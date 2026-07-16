package domain_test

import (
	"testing"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestReconcileCapsuleLockComponentsReportsComponentDivergenceClasses(t *testing.T) {
	lockDigest := sha256Digest('a')
	capsuleDigest := sha256Digest('b')
	lock := domain.CapsuleLockSnapshot{
		Digest: lockDigest,
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:missing":  {ID: "config:missing", CapsuleDigest: capsuleDigest, ComponentDigest: sha256Digest('c')},
			"config:changed":  {ID: "config:changed", CapsuleDigest: capsuleDigest, ComponentDigest: sha256Digest('d')},
			"config:conflict": {ID: "config:conflict", CapsuleDigest: capsuleDigest, ComponentDigest: sha256Digest('e')},
		},
	}
	installed := []domain.InstalledComponentState{
		{ComponentID: "config:changed", LockDigest: lockDigest, CapsuleDigest: capsuleDigest, ComponentDigest: sha256Digest('f')},
		{ComponentID: "config:conflict", LockDigest: sha256Digest('z'), CapsuleDigest: capsuleDigest, ComponentDigest: sha256Digest('e')},
		{ComponentID: "config:extra", LockDigest: lockDigest, CapsuleDigest: capsuleDigest, ComponentDigest: sha256Digest('g')},
	}

	divergences := domain.ReconcileCapsuleLockComponents(lock, installed)
	got := make(map[string]domain.ComponentDivergenceClass, len(divergences))
	for _, divergence := range divergences {
		got[divergence.ComponentID] = divergence.Class
	}
	want := map[string]domain.ComponentDivergenceClass{
		"config:missing":  domain.DivergenceMissing,
		"config:changed":  domain.DivergenceChanged,
		"config:conflict": domain.DivergenceConflict,
		"config:extra":    domain.DivergenceExtra,
	}
	if len(got) != len(want) {
		t.Fatalf("divergences = %#v, want %#v", got, want)
	}
	for id, wantClass := range want {
		if got[id] != wantClass {
			t.Errorf("divergence[%q] = %q, want %q", id, got[id], wantClass)
		}
	}
}

func TestReconcileCapsuleLockComponentsTreatsEmptyInstalledIdentitiesAsMissing(t *testing.T) {
	lock := domain.CapsuleLockSnapshot{
		Digest: sha256Digest('a'),
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:incomplete": {
				ID: "config:incomplete", CapsuleDigest: sha256Digest('b'), ComponentDigest: sha256Digest('c'),
			},
		},
	}
	installed := []domain.InstalledComponentState{{
		ComponentID: "config:incomplete", LockDigest: "", CapsuleDigest: "", ComponentDigest: sha256Digest('c'),
	}}

	divergences := domain.ReconcileCapsuleLockComponents(lock, installed)
	if len(divergences) != 1 || divergences[0].Class != domain.DivergenceMissing {
		t.Fatalf("incomplete installed identity reconciliation = %#v, want one missing divergence", divergences)
	}
}
