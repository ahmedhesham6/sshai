package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	ErrStaleProfileHead       = errors.New("stale Profile head")
	registryCapsuleRefPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$`)
)

type canonicalProfileVersionCapsuleRef struct {
	Ref             string   `json:"ref"`
	FreshnessPolicy string   `json:"freshnessPolicy"`
	Exclusions      []string `json:"exclusions"`
}

// ComputeProfileVersionDigest computes the version digest of record from the
// ordered Capsule Refs. Exclusions are canonicalized as a set within each Ref;
// Capsule Ref order remains significant.
func ComputeProfileVersionDigest(capsuleRefs []CapsuleRef) string {
	canonicalRefs := make([]canonicalProfileVersionCapsuleRef, len(capsuleRefs))
	for index, ref := range capsuleRefs {
		exclusions := append([]string(nil), ref.Exclusions...)
		sort.Strings(exclusions)
		if exclusions == nil {
			exclusions = []string{}
		}
		canonicalRefs[index] = canonicalProfileVersionCapsuleRef{
			Ref: ref.Ref, FreshnessPolicy: string(ref.FreshnessPolicy), Exclusions: exclusions,
		}
	}
	canonicalJSON, _ := json.Marshal(canonicalRefs)
	digest := sha256.Sum256(canonicalJSON)
	return fmt.Sprintf("sha256:%x", digest)
}

// ArtifactTrust identifies provenance metadata for legacy Profile Artifacts.
type ArtifactTrust string

const (
	TrustUserAuthored  ArtifactTrust = "user_authored"
	TrustTrustedSource ArtifactTrust = "trusted_source"
	TrustThirdParty    ArtifactTrust = "third_party"
	TrustUnknown       ArtifactTrust = "unknown"
)

type FreshnessPolicy string

const (
	FreshnessTrack  FreshnessPolicy = "track"
	FreshnessReview FreshnessPolicy = "review"
	FreshnessPin    FreshnessPolicy = "pin"

	FreshnessPolicyTrack  = FreshnessTrack
	FreshnessPolicyReview = FreshnessReview
	FreshnessPolicyPin    = FreshnessPin
)

func (policy FreshnessPolicy) Valid() bool {
	switch policy {
	case FreshnessTrack, FreshnessReview, FreshnessPin:
		return true
	default:
		return false
	}
}

type CapsuleRef struct {
	Ref             string
	FreshnessPolicy FreshnessPolicy
	Exclusions      []string
}

type ProfileSnapshot struct {
	ID          string
	OwnerUserID string
	Name        string
	Slug        string
	CreatedAt   time.Time
	ArchivedAt  *time.Time
}

type Profile struct{ snapshot ProfileSnapshot }

func CreateProfile(snapshot ProfileSnapshot) (Profile, error) {
	if strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.OwnerUserID) == "" ||
		strings.TrimSpace(snapshot.Name) == "" || strings.TrimSpace(snapshot.Slug) == "" {
		return Profile{}, errors.New("create Profile: ID, owner User ID, name, and slug are required")
	}
	if snapshot.CreatedAt.IsZero() {
		return Profile{}, errors.New("create Profile: creation time is required")
	}
	if snapshot.ArchivedAt != nil && snapshot.ArchivedAt.Before(snapshot.CreatedAt) {
		return Profile{}, errors.New("create Profile: archive time is before creation time")
	}
	snapshot.CreatedAt = snapshot.CreatedAt.Round(0).UTC()
	snapshot.ArchivedAt = cloneCanonicalTime(snapshot.ArchivedAt)
	return Profile{snapshot: snapshot}, nil
}

func (profile Profile) Snapshot() ProfileSnapshot {
	snapshot := profile.snapshot
	snapshot.ArchivedAt = cloneCanonicalTime(snapshot.ArchivedAt)
	return snapshot
}

type ProfileVersionSnapshot struct {
	ID              string
	ProfileID       string
	ParentVersionID *string
	Version         int64
	Digest          string
	CapsuleRefs     []CapsuleRef
	CreatedAt       time.Time
}

type ProfileVersion struct{ snapshot ProfileVersionSnapshot }

type ProfileVersionPublication struct {
	ID          string
	Digest      string
	CapsuleRefs []CapsuleRef
	CreatedAt   time.Time
}

// ProfileVersionData is the exported, durable projection used when a
// workflow crosses a Restate action boundary. It intentionally contains only
// the immutable identity and ordered Capsule Refs needed for resolution.
type ProfileVersionData struct {
	ID          string       `json:"id"`
	CapsuleRefs []CapsuleRef `json:"capsuleRefs"`
}

func (profile Profile) PublishVersion(head *ProfileVersion, expectedHeadVersionID *string, publication ProfileVersionPublication) (ProfileVersion, error) {
	profileSnapshot := profile.Snapshot()
	if profileSnapshot.ArchivedAt != nil {
		return ProfileVersion{}, errors.New("publish Profile Version: Profile is archived")
	}
	version := int64(1)
	var parentVersionID *string
	if head == nil {
		if expectedHeadVersionID != nil {
			return ProfileVersion{}, ErrStaleProfileHead
		}
	} else {
		headSnapshot := head.Snapshot()
		if headSnapshot.ProfileID != profileSnapshot.ID {
			return ProfileVersion{}, errors.New("publish Profile Version: head belongs to another Profile")
		}
		if expectedHeadVersionID == nil || *expectedHeadVersionID != headSnapshot.ID {
			return ProfileVersion{}, ErrStaleProfileHead
		}
		parentVersionID = &headSnapshot.ID
		version = headSnapshot.Version + 1
	}
	return publishProfileVersion(ProfileVersionSnapshot{
		ID: publication.ID, ProfileID: profileSnapshot.ID, ParentVersionID: parentVersionID,
		Version: version, Digest: publication.Digest, CapsuleRefs: publication.CapsuleRefs, CreatedAt: publication.CreatedAt,
	})
}

func publishProfileVersion(snapshot ProfileVersionSnapshot) (ProfileVersion, error) {
	if strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.ProfileID) == "" || snapshot.Version < 1 {
		return ProfileVersion{}, errors.New("publish Profile Version: ID, Profile ID, and positive version are required")
	}
	if !contentDigestPattern.MatchString(snapshot.Digest) {
		return ProfileVersion{}, errors.New("publish Profile Version: digest is invalid")
	}
	if snapshot.CreatedAt.IsZero() {
		return ProfileVersion{}, errors.New("publish Profile Version: creation time is required")
	}
	if err := ValidateCapsuleRefs(snapshot.CapsuleRefs); err != nil {
		return ProfileVersion{}, fmt.Errorf("publish Profile Version: %w", err)
	}
	if snapshot.ParentVersionID != nil && strings.TrimSpace(*snapshot.ParentVersionID) == "" {
		return ProfileVersion{}, errors.New("publish Profile Version: parent ID cannot be empty")
	}
	if snapshot.Version == 1 && snapshot.ParentVersionID != nil {
		return ProfileVersion{}, errors.New("publish Profile Version: first version cannot have a parent")
	}
	if snapshot.Version > 1 && snapshot.ParentVersionID == nil {
		return ProfileVersion{}, errors.New("publish Profile Version: later version requires a parent")
	}
	if snapshot.ParentVersionID != nil && *snapshot.ParentVersionID == snapshot.ID {
		return ProfileVersion{}, errors.New("publish Profile Version: version cannot parent itself")
	}
	refs := cloneCapsuleRefs(snapshot.CapsuleRefs)
	snapshot.ParentVersionID = cloneString(snapshot.ParentVersionID)
	snapshot.CapsuleRefs = refs
	snapshot.CreatedAt = snapshot.CreatedAt.Round(0).UTC()
	return ProfileVersion{snapshot: snapshot}, nil
}

// ValidateCapsuleRefs validates the ordered Capsule Ref composition of a
// Profile Version without requiring a Profile aggregate.
func ValidateCapsuleRefs(refs []CapsuleRef) error {
	if len(refs) == 0 {
		return errors.New("Capsule Refs are required")
	}
	seenRefs := make(map[string]struct{}, len(refs))
	for index, ref := range refs {
		if err := validateCapsuleRef(ref); err != nil {
			return fmt.Errorf("Capsule Ref %d: %w", index, err)
		}
		if _, duplicate := seenRefs[ref.Ref]; duplicate {
			return fmt.Errorf("duplicate Capsule Ref %q", ref.Ref)
		}
		seenRefs[ref.Ref] = struct{}{}
	}
	return nil
}

func RestoreProfileVersion(snapshot ProfileVersionSnapshot) (ProfileVersion, error) {
	return publishProfileVersion(snapshot)
}

func validateCapsuleRef(ref CapsuleRef) error {
	if strings.TrimSpace(ref.Ref) == "" || strings.TrimSpace(ref.Ref) != ref.Ref || !registryCapsuleRefPattern.MatchString(ref.Ref) {
		return errors.New("registry reference is invalid")
	}
	if !ref.FreshnessPolicy.Valid() {
		return errors.New("freshness policy is invalid")
	}
	for _, exclusion := range ref.Exclusions {
		if strings.TrimSpace(exclusion) == "" {
			return errors.New("exclusions must contain non-empty component IDs")
		}
	}
	return nil
}

func (version ProfileVersion) Snapshot() ProfileVersionSnapshot {
	snapshot := version.snapshot
	snapshot.ParentVersionID = cloneString(snapshot.ParentVersionID)
	snapshot.CapsuleRefs = cloneCapsuleRefs(snapshot.CapsuleRefs)
	return snapshot
}

func cloneCapsuleRefs(refs []CapsuleRef) []CapsuleRef {
	if refs == nil {
		return nil
	}
	clone := append([]CapsuleRef(nil), refs...)
	for index := range clone {
		clone[index].Exclusions = append([]string(nil), clone[index].Exclusions...)
	}
	return clone
}

func cloneCanonicalTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := value.Round(0).UTC()
	return &clone
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
