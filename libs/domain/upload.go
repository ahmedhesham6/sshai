package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type UploadKind string

const (
	UploadProfileArtifact UploadKind = "profile_artifact"
	UploadGitBundle       UploadKind = "git_bundle"
	UploadTrackedPatch    UploadKind = "tracked_patch"
	UploadUntrackedBundle UploadKind = "untracked_bundle"
	UploadSeedManifest    UploadKind = "seed_manifest"
)

type UploadIntentSnapshot struct {
	ID          string
	OwnerUserID string
	Kind        UploadKind
	Digest      string
	SizeBytes   int64
	ObjectKey   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type UploadIntent struct {
	snapshot UploadIntentSnapshot
}

func ReserveUploadIntent(snapshot UploadIntentSnapshot) (UploadIntent, error) {
	for _, field := range []struct{ name, value string }{
		{name: "ID", value: snapshot.ID},
		{name: "owner User ID", value: snapshot.OwnerUserID},
		{name: "object key", value: snapshot.ObjectKey},
	} {
		if strings.TrimSpace(field.value) == "" {
			return UploadIntent{}, fmt.Errorf("reserve Upload Intent: %s is required", field.name)
		}
	}
	if !snapshot.Kind.valid() {
		return UploadIntent{}, fmt.Errorf("reserve Upload Intent: unknown kind %q", snapshot.Kind)
	}
	if !contentDigestPattern.MatchString(snapshot.Digest) {
		return UploadIntent{}, errors.New("reserve Upload Intent: digest must be a SHA-256 content address")
	}
	if snapshot.SizeBytes < 0 {
		return UploadIntent{}, errors.New("reserve Upload Intent: size cannot be negative")
	}
	if snapshot.CreatedAt.IsZero() {
		return UploadIntent{}, errors.New("reserve Upload Intent: creation time is required")
	}
	if snapshot.ExpiresAt.IsZero() || !snapshot.ExpiresAt.After(snapshot.CreatedAt) {
		return UploadIntent{}, errors.New("reserve Upload Intent: expiry must follow creation")
	}
	snapshot.CreatedAt = snapshot.CreatedAt.UTC()
	snapshot.ExpiresAt = snapshot.ExpiresAt.UTC()
	return UploadIntent{snapshot: snapshot}, nil
}

func (intent UploadIntent) Snapshot() UploadIntentSnapshot {
	return intent.snapshot
}

func (kind UploadKind) valid() bool {
	switch kind {
	case UploadProfileArtifact, UploadGitBundle, UploadTrackedPatch, UploadUntrackedBundle, UploadSeedManifest:
		return true
	default:
		return false
	}
}
