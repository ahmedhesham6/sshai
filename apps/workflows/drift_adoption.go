package workflows

import (
	"context"
	"errors"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

// DriftAdoptionActions is the durable boundary for proposal recording and
// accepted-proposal consumption. It does not expose a Capsule mutation API.
type DriftAdoptionActions interface {
	RecordDriftAdoptionProposal(context.Context, profile.DriftAdoptionProposal) error
	AcceptDriftAdoption(context.Context, profile.AcceptedDriftAdoption) error
}

func SubmitDriftAdoptionProposal(ctx context.Context, ownerID string, proposal profile.DriftAdoptionProposal, actions DriftAdoptionActions) error {
	if actions == nil {
		return errors.New("drift adoption actions are required")
	}
	if ownerID == "" {
		return errors.New("drift adoption authenticated owner is required")
	}
	if proposal.ComponentID == "" || proposal.OwningCapsuleRef == "" || proposal.ObservedDigest == "" || proposal.DiffSummary == "" {
		return errors.New("drift adoption proposal is incomplete")
	}
	parsed, err := contracts.ParseOwnedCapsuleRef(proposal.OwningCapsuleRef)
	if err != nil || parsed.OwnerID != ownerID {
		return errors.New("drift adoption owning Capsule Ref is not self-owned")
	}
	return actions.RecordDriftAdoptionProposal(ctx, proposal)
}

// ConsumeDriftAdoption validates consent at the workflow seam before any
// durable mutation is handed to the repository.
func ConsumeDriftAdoption(ctx context.Context, ownerID string, lock domain.CapsuleLockSnapshot, proposal profile.DriftAdoptionProposal, consent profile.DriftAdoptionConsent, actions DriftAdoptionActions) error {
	if actions == nil {
		return errors.New("drift adoption actions are required")
	}
	accepted, err := profile.AcceptDriftAdoption(lock, ownerID, proposal, consent)
	if err != nil {
		return err
	}
	return actions.AcceptDriftAdoption(ctx, accepted)
}
