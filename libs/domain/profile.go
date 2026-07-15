package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type Sensitivity string

var ErrStaleProfileHead = errors.New("stale Profile head")

const (
	SensitivityPublic     Sensitivity = "public"
	SensitivityPrivate    Sensitivity = "private"
	SensitivityCredential Sensitivity = "credential"
	SensitivityUnknown    Sensitivity = "unknown"
)

type TrustClass string

const (
	TrustUserAuthored  TrustClass = "user_authored"
	TrustTrustedSource TrustClass = "trusted_source"
	TrustThirdParty    TrustClass = "third_party"
	TrustUnknown       TrustClass = "unknown"
)

type ArtifactKind string

const (
	ArtifactAgentInstruction ArtifactKind = "agent_instruction"
	ArtifactCodexSettings    ArtifactKind = "codex_settings"
	ArtifactClaudeSettings   ArtifactKind = "claude_settings"
	ArtifactShellPreferences ArtifactKind = "shell_preferences"
	ArtifactGitPreferences   ArtifactKind = "git_preferences"
	ArtifactSkillInstruction ArtifactKind = "agent_skill_instruction"
	ArtifactSkillExecutable  ArtifactKind = "agent_skill_executable"
)

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

type ProfileArtifact struct {
	ID                 string
	ProfileVersionID   string
	Kind               ArtifactKind
	SourceLocator      string
	SourceDigest       string
	ContentDigest      string
	SizeBytes          int64
	Mode               uint32
	Sensitivity        Sensitivity
	Trust              TrustClass
	ContainsExecutable bool
}

type ProfileVersionSnapshot struct {
	ID              string
	ProfileID       string
	ParentVersionID *string
	Version         int64
	Digest          string
	Artifacts       []ProfileArtifact
	CreatedAt       time.Time
}

type ProfileVersion struct{ snapshot ProfileVersionSnapshot }

type ProfileVersionPublication struct {
	ID        string
	Digest    string
	Artifacts []ProfileArtifact
	CreatedAt time.Time
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
		Version: version, Digest: publication.Digest, Artifacts: publication.Artifacts, CreatedAt: publication.CreatedAt,
	})
}

func publishProfileVersion(snapshot ProfileVersionSnapshot) (ProfileVersion, error) {
	if strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.ProfileID) == "" || snapshot.Version < 1 {
		return ProfileVersion{}, errors.New("publish Profile Version: ID, Profile ID, and positive version are required")
	}
	if !contentDigestPattern.MatchString(snapshot.Digest) {
		return ProfileVersion{}, errors.New("publish Profile Version: digest is invalid")
	}
	if snapshot.CreatedAt.IsZero() || len(snapshot.Artifacts) == 0 {
		return ProfileVersion{}, errors.New("publish Profile Version: creation time and artifacts are required")
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
	seenIDs := make(map[string]struct{}, len(snapshot.Artifacts))
	seenLocators := make(map[string]struct{}, len(snapshot.Artifacts))
	artifacts := append([]ProfileArtifact(nil), snapshot.Artifacts...)
	for index, artifact := range artifacts {
		if err := validateProfileArtifact(snapshot.ID, artifact); err != nil {
			return ProfileVersion{}, fmt.Errorf("publish Profile Version: artifact %d: %w", index, err)
		}
		if _, duplicate := seenIDs[artifact.ID]; duplicate {
			return ProfileVersion{}, fmt.Errorf("publish Profile Version: duplicate artifact ID %q", artifact.ID)
		}
		if _, duplicate := seenLocators[artifact.SourceLocator]; duplicate {
			return ProfileVersion{}, fmt.Errorf("publish Profile Version: duplicate source locator %q", artifact.SourceLocator)
		}
		seenIDs[artifact.ID] = struct{}{}
		seenLocators[artifact.SourceLocator] = struct{}{}
	}
	snapshot.ParentVersionID = cloneString(snapshot.ParentVersionID)
	snapshot.Artifacts = artifacts
	snapshot.CreatedAt = snapshot.CreatedAt.Round(0).UTC()
	return ProfileVersion{snapshot: snapshot}, nil
}

func RestoreProfileVersion(snapshot ProfileVersionSnapshot) (ProfileVersion, error) {
	return publishProfileVersion(snapshot)
}

func validateProfileArtifact(versionID string, artifact ProfileArtifact) error {
	if strings.TrimSpace(artifact.ID) == "" || artifact.ProfileVersionID != versionID || strings.TrimSpace(string(artifact.Kind)) == "" || strings.TrimSpace(artifact.SourceLocator) == "" {
		return errors.New("identity, kind, and source locator are required")
	}
	if !contentDigestPattern.MatchString(artifact.SourceDigest) || !contentDigestPattern.MatchString(artifact.ContentDigest) {
		return errors.New("source and content digests must be SHA-256")
	}
	if artifact.SizeBytes < 0 || artifact.Mode > 0o777 {
		return errors.New("artifact size or file mode is invalid")
	}
	if artifact.Sensitivity != SensitivityPublic && artifact.Sensitivity != SensitivityPrivate {
		return errors.New("credential and unknown sensitivity cannot be published")
	}
	if !artifact.Kind.valid() {
		return errors.New("artifact kind is invalid")
	}
	if artifact.ContainsExecutable != artifact.Kind.containsExecutable() {
		return errors.New("executable classification does not match artifact kind")
	}
	switch artifact.Trust {
	case TrustUserAuthored, TrustTrustedSource, TrustThirdParty:
	default:
		return errors.New("trust classification is invalid")
	}
	return nil
}

func (kind ArtifactKind) valid() bool {
	switch kind {
	case ArtifactAgentInstruction, ArtifactCodexSettings, ArtifactClaudeSettings,
		ArtifactShellPreferences, ArtifactGitPreferences, ArtifactSkillInstruction, ArtifactSkillExecutable:
		return true
	default:
		return false
	}
}

func (kind ArtifactKind) containsExecutable() bool {
	return kind == ArtifactShellPreferences || kind == ArtifactSkillExecutable
}

func (version ProfileVersion) Snapshot() ProfileVersionSnapshot {
	snapshot := version.snapshot
	snapshot.ParentVersionID = cloneString(snapshot.ParentVersionID)
	snapshot.Artifacts = append([]ProfileArtifact(nil), snapshot.Artifacts...)
	return snapshot
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
