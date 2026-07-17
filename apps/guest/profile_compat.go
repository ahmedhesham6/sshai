package guest

import "github.com/ahmedhesham6/sshai/libs/profile"

// Transitional aliases: this vocabulary moved to libs/profile (plans/001).
// New code should import libs/profile directly. Remove once no guest-internal
// reference remains (see plans/002).
type (
	MaterializationMode          = profile.MaterializationMode
	MaterializationRoot          = profile.MaterializationRoot
	ProfileMaterializationResult = profile.ProfileMaterializationResult
	InstalledMaterialization     = profile.InstalledMaterialization
	DriftAdoptionProposal        = profile.DriftAdoptionProposal
	DriftAdoptionConsent         = profile.DriftAdoptionConsent
	AcceptedDriftAdoption        = profile.AcceptedDriftAdoption
)

const (
	MaterializationManaged    = profile.MaterializationManaged
	MaterializationSeeded     = profile.MaterializationSeeded
	MaterializationReferenced = profile.MaterializationReferenced
	MaterializationHome       = profile.MaterializationHome
	MaterializationWorkspace  = profile.MaterializationWorkspace
)

var (
	InstalledMaterializationsFromResults = profile.InstalledMaterializationsFromResults
	ProposeDriftAdoption                 = profile.ProposeDriftAdoption
	AcceptDriftAdoption                  = profile.AcceptDriftAdoption
)
