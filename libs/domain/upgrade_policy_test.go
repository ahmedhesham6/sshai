package domain_test

import (
	"testing"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestEvaluateAutoSafeVetoesEveryRatifiedUnsafeChange(t *testing.T) {
	tests := []struct {
		name string
		plan domain.AutoSafePlan
	}{
		{name: "managed target drift", plan: domain.AutoSafePlan{ManagedTargets: []domain.ManagedTargetState{{ComponentID: "config:editor", DesiredDigest: "sha256:desired", LastAppliedDigest: "sha256:last", ObservedDigest: "sha256:observed"}}}},
		{name: "executable digest change", plan: domain.AutoSafePlan{ManagedTargetsClean: true, ExecutableDigestChanged: true}},
		{name: "permission change", plan: domain.AutoSafePlan{ManagedTargetsClean: true, PermissionChanged: true}},
		{name: "integration change", plan: domain.AutoSafePlan{ManagedTargetsClean: true, IntegrationChanged: true}},
		{name: "credential requirement change", plan: domain.AutoSafePlan{ManagedTargetsClean: true, CredentialRequirementChanged: true}},
		{name: "composition conflict", plan: domain.AutoSafePlan{ManagedTargetsClean: true, HasConflicts: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := domain.EvaluateAutoSafe(test.plan)
			if decision.Applyable {
				t.Fatalf("EvaluateAutoSafe(%#v) = %#v, want veto", test.plan, decision)
			}
			if decision.Reason == "" {
				t.Fatal("auto_safe veto did not identify a reason")
			}
		})
	}
}

func TestEvaluateAutoSafeAllowsOnlyCleanDeclarativeNonConflictingPlan(t *testing.T) {
	decision := domain.EvaluateAutoSafe(domain.AutoSafePlan{ManagedTargetsClean: true})
	if !decision.Applyable || decision.Reason != "" {
		t.Fatalf("EvaluateAutoSafe(clean) = %#v, want applyable", decision)
	}
}

func TestUpgradePolicyAcceptsOnlyManualNotifyAndAutoSafe(t *testing.T) {
	for _, policy := range []domain.UpgradePolicy{domain.UpgradeManual, domain.UpgradeNotify, domain.UpgradeAutoSafe} {
		if !policy.Valid() {
			t.Errorf("UpgradePolicy %q is invalid", policy)
		}
	}
	if domain.UpgradePolicy("always").Valid() {
		t.Fatal("unknown UpgradePolicy accepted")
	}
}
