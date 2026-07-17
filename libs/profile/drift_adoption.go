package profile

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

var driftAdoptionDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type DriftAdoptionProposal struct {
	ComponentID      string
	OwningCapsuleRef string
	ObservedDigest   string
	DiffSummary      string
	RequiresReview   bool
	ConsentRequired  bool
}

type DriftAdoptionConsent struct {
	Accepted                 bool
	ExecutableReviewApproved bool
}

type AcceptedDriftAdoption struct {
	Proposal DriftAdoptionProposal
	Accepted bool
}

// ProposeDriftAdoption only creates a structured proposal. It does not update
// the installed record or the owning Capsule.
func ProposeDriftAdoption(lock domain.CapsuleLockSnapshot, ownerID string, installed InstalledMaterialization, observedDigest, diffSummary string) (DriftAdoptionProposal, error) {
	if ownerID == "" {
		return DriftAdoptionProposal{}, errors.New("drift adoption authenticated owner is required")
	}
	if installed.Mode != MaterializationManaged {
		return DriftAdoptionProposal{}, errors.New("drift adoption requires a managed Component")
	}
	if installed.ComponentID == "" {
		return DriftAdoptionProposal{}, errors.New("drift adoption Component ID is required")
	}
	if !driftAdoptionDigestPattern.MatchString(observedDigest) {
		return DriftAdoptionProposal{}, errors.New("drift adoption observed digest is invalid")
	}
	if installed.LastAppliedDigest != "" && observedDigest == installed.LastAppliedDigest {
		return DriftAdoptionProposal{}, errors.New("drift adoption requires observed managed drift")
	}
	if diffSummary == "" {
		return DriftAdoptionProposal{}, errors.New("drift adoption diff summary is required")
	}
	locked, present := lock.ResolvedComponents[installed.ComponentID]
	if !present {
		return DriftAdoptionProposal{}, fmt.Errorf("drift adoption Component %q is not in the Capsule Lock", installed.ComponentID)
	}
	if installed.CapsuleDigest != "" && installed.CapsuleDigest != locked.CapsuleDigest {
		return DriftAdoptionProposal{}, errors.New("drift adoption owning Capsule does not match the Lock")
	}
	owningRef := ""
	for _, capsule := range lock.Capsules {
		if capsule.Digest == locked.CapsuleDigest {
			owningRef = capsule.Ref
			break
		}
	}
	if owningRef == "" {
		return DriftAdoptionProposal{}, fmt.Errorf("drift adoption owning Capsule %q is not listed in the Lock", locked.CapsuleDigest)
	}
	if err := validateOwnedCapsuleRef(ownerID, owningRef); err != nil {
		return DriftAdoptionProposal{}, fmt.Errorf("drift adoption owning Capsule Ref: %w", err)
	}
	return DriftAdoptionProposal{
		ComponentID: installed.ComponentID, OwningCapsuleRef: owningRef, ObservedDigest: observedDigest,
		DiffSummary: diffSummary, RequiresReview: driftComponentRequiresReview(locked),
		ConsentRequired: true,
	}, nil
}

// AcceptDriftAdoption validates explicit consent and returns an immutable
// accepted proposal. Publishing a new Capsule version is a separate caller
// action; this function deliberately performs no mutation.
func AcceptDriftAdoption(lock domain.CapsuleLockSnapshot, ownerID string, proposal DriftAdoptionProposal, consent DriftAdoptionConsent) (AcceptedDriftAdoption, error) {
	if ownerID == "" {
		return AcceptedDriftAdoption{}, errors.New("drift adoption authenticated owner is required")
	}
	if proposal.ComponentID == "" || proposal.OwningCapsuleRef == "" || proposal.ObservedDigest == "" || proposal.DiffSummary == "" {
		return AcceptedDriftAdoption{}, errors.New("drift adoption proposal is incomplete")
	}
	if !driftAdoptionDigestPattern.MatchString(proposal.ObservedDigest) {
		return AcceptedDriftAdoption{}, errors.New("drift adoption observed digest is invalid")
	}
	if !consent.Accepted {
		return AcceptedDriftAdoption{}, errors.New("drift adoption requires explicit consent")
	}
	locked, present := lock.ResolvedComponents[proposal.ComponentID]
	if !present {
		return AcceptedDriftAdoption{}, fmt.Errorf("drift adoption Component %q is not in the Capsule Lock", proposal.ComponentID)
	}
	owningRef := ""
	for _, capsule := range lock.Capsules {
		if capsule.Digest == locked.CapsuleDigest {
			owningRef = capsule.Ref
			break
		}
	}
	if owningRef == "" || proposal.OwningCapsuleRef != owningRef {
		return AcceptedDriftAdoption{}, errors.New("drift adoption owning Capsule Ref does not match the Lock")
	}
	if err := validateOwnedCapsuleRef(ownerID, proposal.OwningCapsuleRef); err != nil {
		return AcceptedDriftAdoption{}, fmt.Errorf("drift adoption owning Capsule Ref: %w", err)
	}
	requiresReview := driftComponentRequiresReview(locked)
	if requiresReview && !consent.ExecutableReviewApproved {
		return AcceptedDriftAdoption{}, errors.New("executable drift adoption requires explicit review")
	}
	proposal.RequiresReview = requiresReview
	proposal.ConsentRequired = true
	return AcceptedDriftAdoption{Proposal: proposal, Accepted: true}, nil
}

func driftComponentRequiresReview(component domain.ResolvedComponent) bool {
	return component.TrustClass == domain.TrustExecutable ||
		component.TrustClass == domain.TrustPermission ||
		component.Type == domain.ComponentIntegration ||
		component.Type == domain.ComponentPermissionPolicy ||
		component.Type == domain.ComponentHook ||
		component.Type == domain.ComponentExtension
}

func validateOwnedCapsuleRef(ownerID, ref string) error {
	parsed, err := contracts.ParseOwnedCapsuleRef(ref)
	if err != nil {
		return err
	}
	if parsed.OwnerID != ownerID {
		return fmt.Errorf("Capsule Ref %q belongs to owner %q, want %q", ref, parsed.OwnerID, ownerID)
	}
	return nil
}
