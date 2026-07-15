package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrProfileConflict           = errors.New("Profile already exists")
	ErrInvalidProfilePublication = errors.New("invalid Profile publication")
)

type profileCreateInput struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type profilePublicationInput struct {
	ProfileID             string                 `json:"profileId"`
	ExpectedHeadVersionID *string                `json:"expectedHeadVersionId"`
	Digest                string                 `json:"digest"`
	Artifacts             []profileArtifactInput `json:"artifacts"`
}

type profileArtifactInput struct {
	Kind               domain.ArtifactKind `json:"kind"`
	SourceLocator      string              `json:"sourceLocator"`
	SourceDigest       string              `json:"sourceDigest"`
	ContentDigest      string              `json:"contentDigest"`
	SizeBytes          int64               `json:"sizeBytes"`
	Mode               uint32              `json:"mode"`
	Sensitivity        domain.Sensitivity  `json:"sensitivity"`
	Trust              domain.TrustClass   `json:"trust"`
	ContainsExecutable bool                `json:"containsExecutable"`
}

func (store *Store) CreateProfile(ctx context.Context, candidate domain.Profile, idempotencyKey string) (domain.Profile, error) {
	snapshot := candidate.Snapshot()
	input, err := json.Marshal(profileCreateInput{Name: snapshot.Name, Slug: snapshot.Slug})
	if err != nil {
		return domain.Profile{}, fmt.Errorf("create Profile: encode input: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Profile{}, fmt.Errorf("create Profile: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	key := dbsql.LockProfileCreateRegistrationParams{OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey}
	if _, err := queries.LockProfileCreateRegistration(ctx, key); err != nil {
		return domain.Profile{}, fmt.Errorf("create Profile: lock idempotency key: %w", err)
	}
	existing, err := queries.GetProfileCreateRegistration(ctx, dbsql.GetProfileCreateRegistrationParams(key))
	if err == nil {
		if !sameJSON(existing.Input, input) {
			return domain.Profile{}, ErrIdempotencyConflict
		}
		profile, err := restoreProfile(existing.ID, existing.OwnerUserID, existing.Name, existing.Slug, existing.CreatedAt, existing.ArchivedAt)
		if err != nil {
			return domain.Profile{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Profile{}, fmt.Errorf("create Profile: commit replay: %w", err)
		}
		return profile, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Profile{}, fmt.Errorf("create Profile: find idempotency key: %w", err)
	}
	if err := queries.InsertProfile(ctx, dbsql.InsertProfileParams{
		ID: snapshot.ID, OwnerUserID: snapshot.OwnerUserID, Name: snapshot.Name, Slug: snapshot.Slug,
		CreatedAt: timestamp(snapshot.CreatedAt), ArchivedAt: optionalTimestamp(snapshot.ArchivedAt),
	}); err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.ConstraintName == "profiles_owner_slug_active_key" {
			return domain.Profile{}, ErrProfileConflict
		}
		return domain.Profile{}, fmt.Errorf("create Profile: insert: %w", err)
	}
	if err := queries.InsertProfileCreateRegistration(ctx, dbsql.InsertProfileCreateRegistrationParams{
		OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey, Input: input, ProfileID: snapshot.ID,
	}); err != nil {
		return domain.Profile{}, fmt.Errorf("create Profile: register idempotency key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Profile{}, fmt.Errorf("create Profile: commit: %w", err)
	}
	return candidate, nil
}

func (store *Store) PublishProfileVersion(ctx context.Context, ownerID, profileID string, expectedHeadVersionID *string, publication domain.ProfileVersionPublication, idempotencyKey string) (domain.ProfileVersion, error) {
	input, err := encodeProfilePublication(profileID, expectedHeadVersionID, publication)
	if err != nil {
		return domain.ProfileVersion{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	key := dbsql.LockProfilePublicationRegistrationParams{OwnerUserID: ownerID, IdempotencyKey: idempotencyKey}
	if _, err := queries.LockProfilePublicationRegistration(ctx, key); err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: lock idempotency key: %w", err)
	}
	registration, err := queries.GetProfilePublicationRegistration(ctx, dbsql.GetProfilePublicationRegistrationParams(key))
	if err == nil {
		if !sameJSON(registration.Input, input) {
			return domain.ProfileVersion{}, ErrIdempotencyConflict
		}
		version, err := loadProfileVersion(ctx, queries, registration.ProfileID, registration.ProfileVersionID)
		if err != nil {
			return domain.ProfileVersion{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: commit replay: %w", err)
		}
		return version, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: find idempotency key: %w", err)
	}
	row, err := queries.GetOwnedProfileForUpdate(ctx, dbsql.GetOwnedProfileForUpdateParams{ProfileID: profileID, OwnerUserID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProfileVersion{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: lock Profile: %w", err)
	}
	profile, err := restoreProfile(row.ID, row.OwnerUserID, row.Name, row.Slug, row.CreatedAt, row.ArchivedAt)
	if err != nil {
		return domain.ProfileVersion{}, err
	}
	head, err := loadProfileHead(ctx, queries, profileID)
	if err != nil {
		return domain.ProfileVersion{}, err
	}
	version, err := profile.PublishVersion(head, expectedHeadVersionID, publication)
	if err != nil {
		if errors.Is(err, domain.ErrStaleProfileHead) {
			return domain.ProfileVersion{}, err
		}
		return domain.ProfileVersion{}, fmt.Errorf("%w: %v", ErrInvalidProfilePublication, err)
	}
	if err := insertProfileVersion(ctx, queries, version); err != nil {
		return domain.ProfileVersion{}, err
	}
	if err := queries.InsertProfilePublicationRegistration(ctx, dbsql.InsertProfilePublicationRegistrationParams{
		OwnerUserID: ownerID, IdempotencyKey: idempotencyKey, Input: input,
		ProfileID: profileID, ProfileVersionID: version.Snapshot().ID,
	}); err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: register idempotency key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: commit: %w", err)
	}
	return version, nil
}

func encodeProfilePublication(profileID string, expectedHeadVersionID *string, publication domain.ProfileVersionPublication) ([]byte, error) {
	artifacts := make([]profileArtifactInput, len(publication.Artifacts))
	for index, artifact := range publication.Artifacts {
		artifacts[index] = profileArtifactInput{
			Kind: artifact.Kind, SourceLocator: artifact.SourceLocator, SourceDigest: artifact.SourceDigest,
			ContentDigest: artifact.ContentDigest, SizeBytes: artifact.SizeBytes, Mode: artifact.Mode,
			Sensitivity: artifact.Sensitivity, Trust: artifact.Trust,
			ContainsExecutable: artifact.ContainsExecutable,
		}
	}
	input, err := json.Marshal(profilePublicationInput{
		ProfileID: profileID, ExpectedHeadVersionID: expectedHeadVersionID, Digest: publication.Digest, Artifacts: artifacts,
	})
	if err != nil {
		return nil, fmt.Errorf("publish Profile Version: encode input: %w", err)
	}
	return input, nil
}

func loadProfileHead(ctx context.Context, queries *dbsql.Queries, profileID string) (*domain.ProfileVersion, error) {
	row, err := queries.GetProfileHead(ctx, profileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("publish Profile Version: load head: %w", err)
	}
	version, err := restoreProfileVersion(ctx, queries, row)
	return &version, err
}

func loadProfileVersion(ctx context.Context, queries *dbsql.Queries, profileID, versionID string) (domain.ProfileVersion, error) {
	row, err := queries.GetProfileVersion(ctx, dbsql.GetProfileVersionParams{ProfileVersionID: versionID, ProfileID: profileID})
	if err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: load replay: %w", err)
	}
	return restoreProfileVersion(ctx, queries, row)
}

func restoreProfileVersion(ctx context.Context, queries *dbsql.Queries, row dbsql.ProfileVersion) (domain.ProfileVersion, error) {
	if !row.CreatedAt.Valid {
		return domain.ProfileVersion{}, errors.New("restore Profile Version: database returned invalid creation time")
	}
	artifactRows, err := queries.ListProfileArtifacts(ctx, row.ID)
	if err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("restore Profile Version: load artifacts: %w", err)
	}
	artifacts := make([]domain.ProfileArtifact, len(artifactRows))
	for index, artifact := range artifactRows {
		artifacts[index] = domain.ProfileArtifact{
			ID: artifact.ID, ProfileVersionID: artifact.ProfileVersionID, Kind: domain.ArtifactKind(artifact.Kind),
			SourceLocator: artifact.SourceLocator, SourceDigest: artifact.SourceDigest, ContentDigest: artifact.ContentDigest,
			SizeBytes: artifact.SizeBytes, Mode: uint32(artifact.Mode),
			Sensitivity: domain.Sensitivity(artifact.Sensitivity), Trust: domain.TrustClass(artifact.Trust),
			ContainsExecutable: artifact.ContainsExecutable,
		}
	}
	version, err := domain.RestoreProfileVersion(domain.ProfileVersionSnapshot{
		ID: row.ID, ProfileID: row.ProfileID, ParentVersionID: row.ParentVersionID, Version: row.Version,
		Digest: row.Digest, Artifacts: artifacts, CreatedAt: row.CreatedAt.Time,
	})
	if err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("restore Profile Version: %w", err)
	}
	return version, nil
}

func insertProfileVersion(ctx context.Context, queries *dbsql.Queries, version domain.ProfileVersion) error {
	snapshot := version.Snapshot()
	if err := queries.InsertProfileVersion(ctx, dbsql.InsertProfileVersionParams{
		ID: snapshot.ID, ProfileID: snapshot.ProfileID, ParentVersionID: snapshot.ParentVersionID,
		Version: snapshot.Version, Digest: snapshot.Digest, CreatedAt: timestamp(snapshot.CreatedAt),
	}); err != nil {
		return fmt.Errorf("publish Profile Version: insert version: %w", err)
	}
	for _, artifact := range snapshot.Artifacts {
		if err := queries.InsertProfileArtifact(ctx, dbsql.InsertProfileArtifactParams{
			ID: artifact.ID, ProfileVersionID: artifact.ProfileVersionID, Kind: string(artifact.Kind),
			SourceLocator: artifact.SourceLocator, SourceDigest: artifact.SourceDigest, ContentDigest: artifact.ContentDigest,
			SizeBytes: artifact.SizeBytes, Mode: int32(artifact.Mode),
			Sensitivity: string(artifact.Sensitivity), Trust: string(artifact.Trust), ContainsExecutable: artifact.ContainsExecutable,
		}); err != nil {
			return fmt.Errorf("publish Profile Version: insert artifact: %w", err)
		}
	}
	return nil
}

func restoreProfile(id, ownerID, name, slug string, createdAt, archivedAt pgtype.Timestamptz) (domain.Profile, error) {
	if !createdAt.Valid {
		return domain.Profile{}, errors.New("restore Profile: database returned invalid creation time")
	}
	var archived *time.Time
	if archivedAt.Valid {
		archived = &archivedAt.Time
	}
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: id, OwnerUserID: ownerID, Name: name, Slug: slug, CreatedAt: createdAt.Time, ArchivedAt: archived,
	})
	if err != nil {
		return domain.Profile{}, fmt.Errorf("restore Profile: %w", err)
	}
	return profile, nil
}
