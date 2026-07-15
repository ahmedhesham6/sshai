package profile

import (
	"errors"
	"fmt"
)

// DigestState is an immutable observation of whether content exists and, when
// it does, its digest. An absent state is distinct from an empty digest.
type DigestState struct {
	present bool
	digest  string
}

// PresentDigest constructs a present digest observation.
func PresentDigest(digest string) (DigestState, error) {
	if digest == "" {
		return DigestState{}, errors.New("digest must not be empty")
	}
	return DigestState{present: true, digest: digest}, nil
}

// AbsentDigest constructs an observation that content does not exist.
func AbsentDigest() DigestState { return DigestState{} }

type materializationMode uint8

const (
	modeManaged materializationMode = iota + 1
	modeSeeded
	modeReferenced
)

// MaterializationSnapshot is an immutable planning input.
type MaterializationSnapshot struct {
	mode        materializationMode
	desired     DigestState
	lastApplied DigestState
	observed    DigestState
	requirement RequirementState
}

// RequirementState records whether an Environment has supplied the external
// authorization needed by referenced content. It never contains credentials.
type RequirementState string

const (
	RequirementNeedsInput RequirementState = "needs_input"
	RequirementBound      RequirementState = "bound"
)

// NewReferencedMaterialization records a reference to external authorization
// without accepting or retaining credential values.
func NewReferencedMaterialization(
	requirement DigestState,
	observed DigestState,
	state RequirementState,
) (MaterializationSnapshot, error) {
	if state != RequirementNeedsInput && state != RequirementBound {
		return MaterializationSnapshot{}, fmt.Errorf("unsupported requirement state %q", state)
	}
	return MaterializationSnapshot{
		mode:        modeReferenced,
		desired:     requirement,
		observed:    observed,
		requirement: state,
	}, nil
}

// NewManagedMaterialization records managed desired, last-applied, and
// observed state for three-way planning.
func NewManagedMaterialization(desired, lastApplied, observed DigestState) MaterializationSnapshot {
	return MaterializationSnapshot{
		mode:        modeManaged,
		desired:     desired,
		lastApplied: lastApplied,
		observed:    observed,
	}
}

// NewSeededMaterialization records content that is created once and then
// owned entirely by the Environment.
func NewSeededMaterialization(desired, lastApplied, observed DigestState) MaterializationSnapshot {
	return MaterializationSnapshot{
		mode:        modeSeeded,
		desired:     desired,
		lastApplied: lastApplied,
		observed:    observed,
	}
}

// PlanIntent distinguishes normal reconciliation from an explicit prune.
type PlanIntent string

const (
	IntentReconcile PlanIntent = "reconcile"
	IntentPrune     PlanIntent = "prune"
)

// PlanOperation describes the action selected by materialization planning.
type PlanOperation string

const (
	OperationCreate        PlanOperation = "create"
	OperationUpdate        PlanOperation = "update"
	OperationSkip          PlanOperation = "skip"
	OperationDrift         PlanOperation = "drift"
	OperationConflict      PlanOperation = "conflict"
	OperationOrphan        PlanOperation = "orphan"
	OperationRemove        PlanOperation = "remove"
	OperationRequiresInput PlanOperation = "requires_input"
)

// PlanMaterialization selects one safe operation from an immutable snapshot.
func PlanMaterialization(snapshot MaterializationSnapshot, intent PlanIntent) (PlanOperation, error) {
	if intent != IntentReconcile && intent != IntentPrune {
		return "", fmt.Errorf("unsupported plan intent %q", intent)
	}
	switch snapshot.mode {
	case modeManaged, modeSeeded, modeReferenced:
	default:
		return "", errors.New("unsupported materialization mode")
	}
	if !snapshot.desired.present {
		if intent == IntentPrune {
			if snapshot.observed.present {
				return OperationRemove, nil
			}
			return OperationSkip, nil
		}
		return OperationOrphan, nil
	}
	if intent == IntentPrune {
		return "", errors.New("cannot prune a materialization whose source is present")
	}
	switch snapshot.mode {
	case modeReferenced:
		if snapshot.requirement == RequirementNeedsInput {
			return OperationRequiresInput, nil
		}
		return OperationSkip, nil
	case modeSeeded:
		if snapshot.lastApplied.present {
			return OperationSkip, nil
		}
		if snapshot.observed.present {
			return OperationConflict, nil
		}
		return OperationCreate, nil
	case modeManaged:
		if snapshot.observed == snapshot.lastApplied {
			if snapshot.desired == snapshot.lastApplied {
				return OperationSkip, nil
			}
			if snapshot.observed.present {
				return OperationUpdate, nil
			}
			return OperationCreate, nil
		}
		if snapshot.desired == snapshot.lastApplied {
			return OperationDrift, nil
		}
		return OperationConflict, nil
	default:
		return "", errors.New("unsupported materialization mode")
	}
}
