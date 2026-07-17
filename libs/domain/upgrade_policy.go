package domain

import "strings"

// UpgradePolicy controls whether a resolved candidate may be applied without
// a separate user action.
type UpgradePolicy string

const (
	UpgradeManual   UpgradePolicy = "manual"
	UpgradeNotify   UpgradePolicy = "notify"
	UpgradeAutoSafe UpgradePolicy = "auto_safe"

	UpgradePolicyManual   = UpgradeManual
	UpgradePolicyNotify   = UpgradeNotify
	UpgradePolicyAutoSafe = UpgradeAutoSafe
)

func (policy UpgradePolicy) Valid() bool {
	switch policy {
	case UpgradeManual, UpgradeNotify, UpgradeAutoSafe:
		return true
	default:
		return false
	}
}

type AutoSafePlan struct {
	// ManagedTargets is persisted three-way state. ManagedTargetsClean is kept
	// for wire compatibility but is intentionally ignored by EvaluateAutoSafe.
	ManagedTargets               []ManagedTargetState
	ManagedTargetsClean          bool
	ExecutableDigestChanged      bool
	PermissionChanged            bool
	IntegrationChanged           bool
	CredentialRequirementChanged bool
	HasConflicts                 bool
}

// ManagedTargetState is the trusted state needed to determine whether a
// managed update may use the automatic path. The caller supplies observations
// and the last-applied/desired identities; it does not supply a verdict.
type ManagedTargetState struct {
	ComponentID       string
	DesiredDigest     string
	LastAppliedDigest string
	ObservedDigest    string
}

// ManagedTargetsAreClean derives cleanliness from the persisted three-way
// state. An empty set means the Environment has no managed targets. A target
// with an incomplete identity is not clean.
func ManagedTargetsAreClean(states []ManagedTargetState) bool {
	for _, state := range states {
		if strings.TrimSpace(state.ComponentID) == "" || strings.TrimSpace(state.DesiredDigest) == "" || strings.TrimSpace(state.LastAppliedDigest) == "" || strings.TrimSpace(state.ObservedDigest) == "" {
			return false
		}
		if state.ObservedDigest != state.LastAppliedDigest {
			return false
		}
	}
	return true
}

type AutoSafeDecision struct {
	Applyable bool
	Reason    string
}

func EvaluateAutoSafe(plan AutoSafePlan) AutoSafeDecision {
	switch {
	case !ManagedTargetsAreClean(plan.ManagedTargets):
		return AutoSafeDecision{Reason: "managed_targets_not_clean"}
	case plan.ExecutableDigestChanged:
		return AutoSafeDecision{Reason: "executable_digest_changed"}
	case plan.PermissionChanged:
		return AutoSafeDecision{Reason: "permission_changed"}
	case plan.IntegrationChanged:
		return AutoSafeDecision{Reason: "integration_changed"}
	case plan.CredentialRequirementChanged:
		return AutoSafeDecision{Reason: "credential_requirement_changed"}
	case plan.HasConflicts:
		return AutoSafeDecision{Reason: "composition_conflict"}
	default:
		return AutoSafeDecision{Applyable: true}
	}
}

func IsAutoSafeApplyable(plan AutoSafePlan) bool {
	return EvaluateAutoSafe(plan).Applyable
}
