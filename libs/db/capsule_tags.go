package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	capsuleTagPartPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	capsuleTagDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// CapsuleTag is an owner-scoped mutable pointer to immutable Capsule content.
type CapsuleTag struct {
	OwnerUserID string
	Name        string
	Tag         string
	Digest      string
	UpdatedAt   time.Time
}

// PutCapsuleTag creates or retargets one owner-scoped Capsule tag. Repeating
// the same digest is idempotent and preserves the original update time.
func (store *Store) PutCapsuleTag(ctx context.Context, ownerID, name, tag, digest string, updatedAt time.Time) (CapsuleTag, error) {
	if err := validateCapsuleTagKey(ownerID, name, tag); err != nil {
		return CapsuleTag{}, fmt.Errorf("put Capsule tag: %w", err)
	}
	if !capsuleTagDigestPattern.MatchString(digest) {
		return CapsuleTag{}, errors.New("put Capsule tag: Capsule digest must be a canonical sha256 digest")
	}
	if updatedAt.IsZero() {
		return CapsuleTag{}, errors.New("put Capsule tag: update time is required")
	}
	row, err := store.queries.UpsertCapsuleTag(ctx, dbsql.UpsertCapsuleTagParams{
		OwnerUserID: ownerID, Name: name, Tag: tag, Digest: digest,
		UpdatedAt: pgtype.Timestamptz{Time: updatedAt.UTC(), Valid: true},
	})
	if err != nil {
		return CapsuleTag{}, fmt.Errorf("put Capsule tag: %w", err)
	}
	return capsuleTagFromRow(row.OwnerUserID, row.Name, row.Tag, row.Digest, row.UpdatedAt)
}

// GetCapsuleTag resolves one owner-scoped Capsule tag. An absent tag reports
// ErrReferenceNotOwned so callers use the same 404 response for foreign tags.
func (store *Store) GetCapsuleTag(ctx context.Context, ownerID, name, tag string) (CapsuleTag, error) {
	if err := validateCapsuleTagKey(ownerID, name, tag); err != nil {
		return CapsuleTag{}, fmt.Errorf("get Capsule tag: %w", err)
	}
	row, err := store.queries.GetCapsuleTag(ctx, dbsql.GetCapsuleTagParams{OwnerUserID: ownerID, Name: name, Tag: tag})
	if errors.Is(err, pgx.ErrNoRows) {
		return CapsuleTag{}, ErrReferenceNotOwned
	}
	if err != nil {
		return CapsuleTag{}, fmt.Errorf("get Capsule tag: %w", err)
	}
	return capsuleTagFromRow(row.OwnerUserID, row.Name, row.Tag, row.Digest, row.UpdatedAt)
}

// ResolveCapsuleTag implements the OCI resolver's narrow TagIndex seam.
func (store *Store) ResolveCapsuleTag(ctx context.Context, ownerID, name, tag string) (string, error) {
	record, err := store.GetCapsuleTag(ctx, ownerID, name, tag)
	if err != nil {
		return "", err
	}
	return record.Digest, nil
}

func validateCapsuleTagKey(ownerID, name, tag string) error {
	if strings.TrimSpace(ownerID) == "" || ownerID != strings.TrimSpace(ownerID) {
		return errors.New("canonical owner User ID is required")
	}
	if !capsuleTagPartPattern.MatchString(name) || !capsuleTagPartPattern.MatchString(tag) {
		return errors.New("Capsule name and tag must be canonical")
	}
	return nil
}

func capsuleTagFromRow(ownerID, name, tag, digest string, updatedAt pgtype.Timestamptz) (CapsuleTag, error) {
	if !updatedAt.Valid {
		return CapsuleTag{}, errors.New("restore Capsule tag: database returned an invalid update time")
	}
	return CapsuleTag{OwnerUserID: ownerID, Name: name, Tag: tag, Digest: digest, UpdatedAt: updatedAt.Time}, nil
}
