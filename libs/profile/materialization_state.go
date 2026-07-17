package profile

import (
	"github.com/ahmedhesham6/sshai/libs/domain"
)

type MaterializationMode string

const (
	MaterializationManaged    MaterializationMode = "managed"
	MaterializationSeeded     MaterializationMode = "seeded"
	MaterializationReferenced MaterializationMode = "referenced"
)

type MaterializationRoot string

const (
	MaterializationHome      MaterializationRoot = "home"
	MaterializationWorkspace MaterializationRoot = "workspace"
)

type ProfileMaterializationResult struct {
	ID                          string
	LockID                      string
	LockDigest                  string
	CapsuleDigest               string
	ComponentID                 string
	ComponentDigest             string
	Adapter                     string
	AdapterID                   string
	AdapterVersion              string
	TargetAgentVersion          string
	Scope                       domain.ComponentScope
	NonSecretOverridesDigest    string
	SecretVersionIdentifiers    []string
	EffectiveCacheKey           string
	Mode                        MaterializationMode
	Root                        MaterializationRoot
	Target                      string
	Selector                    string
	Directory                   bool
	FilePaths                   []string
	DesiredDigest               string
	LastAppliedDigest           string
	ObservedDigest              string
	Operation                   PlanOperation
	RequirementState            RequirementState
	ApprovalRequired            bool
	ApprovalReason              string
	CredentialRequirementDigest string
}

// InstalledMaterialization is the durable state needed to plan the next
// lock. It deliberately stores cache-key fields, never resolved secret values.
type InstalledMaterialization struct {
	ID                          string
	LockID                      string
	LockDigest                  string
	CapsuleDigest               string
	ComponentID                 string
	ComponentDigest             string
	AdapterID                   string
	AdapterVersion              string
	TargetAgentVersion          string
	Scope                       domain.ComponentScope
	NonSecretOverridesDigest    string
	SecretVersionIdentifiers    []string
	EffectiveCacheKey           string
	Mode                        MaterializationMode
	Root                        MaterializationRoot
	Target                      string
	Selector                    string
	Directory                   bool
	FilePaths                   []string
	LastAppliedDigest           string
	ObservedDigest              string
	CredentialRequirementDigest string
}

// InstalledMaterializationsFromResults is a small state bridge for callers
// that persist the guest result in their Environment state store.
func InstalledMaterializationsFromResults(results []ProfileMaterializationResult) []InstalledMaterialization {
	installed := make([]InstalledMaterialization, 0, len(results))
	for _, result := range results {
		installed = append(installed, InstalledMaterialization{
			ID: result.ID, LockID: result.LockID, LockDigest: result.LockDigest, CapsuleDigest: result.CapsuleDigest,
			ComponentID: result.ComponentID, ComponentDigest: result.ComponentDigest,
			AdapterID: result.Adapter, AdapterVersion: result.AdapterVersion,
			TargetAgentVersion: result.TargetAgentVersion, Scope: result.Scope,
			NonSecretOverridesDigest: result.NonSecretOverridesDigest,
			SecretVersionIdentifiers: append([]string(nil), result.SecretVersionIdentifiers...),
			EffectiveCacheKey:        result.EffectiveCacheKey,
			Mode:                     result.Mode, Root: result.Root, Target: result.Target, Selector: result.Selector,
			Directory: result.Directory, FilePaths: append([]string(nil), result.FilePaths...),
			LastAppliedDigest: result.LastAppliedDigest, ObservedDigest: result.ObservedDigest,
			CredentialRequirementDigest: result.CredentialRequirementDigest,
		})
	}
	return installed
}
