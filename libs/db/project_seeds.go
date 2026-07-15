package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

type projectSeedInput struct {
	RepositoryURL         string `json:"repositoryUrl"`
	BaseRevision          string `json:"baseRevision"`
	Digest                string `json:"digest"`
	GitBundleDigest       string `json:"gitBundleDigest,omitempty"`
	TrackedPatchDigest    string `json:"trackedPatchDigest,omitempty"`
	UntrackedBundleDigest string `json:"untrackedBundleDigest,omitempty"`
	ManifestDigest        string `json:"manifestDigest"`
}

func (store *Store) RegisterProjectSeed(ctx context.Context, candidate domain.ProjectSeed, idempotencyKey string) (domain.ProjectSeed, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return domain.ProjectSeed{}, errors.New("register Project Seed: idempotency key is required")
	}
	snapshot := candidate.Snapshot()
	input, err := json.Marshal(projectSeedInputFromSnapshot(snapshot))
	if err != nil {
		return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: encode input: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	if _, err := queries.LockProjectSeedRegistration(ctx, dbsql.LockProjectSeedRegistrationParams{
		OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey,
	}); err != nil {
		return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: lock idempotency key: %w", err)
	}

	existing, err := queries.GetProjectSeedRegistration(ctx, dbsql.GetProjectSeedRegistrationParams{
		OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey,
	})
	if err == nil {
		if !sameJSON(existing.Input, input) {
			return domain.ProjectSeed{}, ErrIdempotencyConflict
		}
		seed, err := domain.RegisterProjectSeed(domain.ProjectSeedSnapshot{
			ID: existing.ID, OwnerUserID: existing.OwnerUserID, RepositoryURL: existing.RepositoryUrl,
			BaseRevision: existing.BaseRevision, Digest: existing.Digest,
			GitBundleDigest:       optionalStringValue(existing.GitBundleDigest),
			TrackedPatchDigest:    optionalStringValue(existing.TrackedPatchDigest),
			UntrackedBundleDigest: optionalStringValue(existing.UntrackedBundleDigest),
			ManifestDigest:        existing.ManifestDigest, CreatedAt: existing.CreatedAt.Time,
		})
		if err != nil {
			return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: restore replay: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: commit replay: %w", err)
		}
		return seed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: read idempotency key: %w", err)
	}

	if _, err := queries.LockProjectSeedDigest(ctx, dbsql.LockProjectSeedDigestParams{
		OwnerUserID: snapshot.OwnerUserID, Digest: snapshot.Digest,
	}); err != nil {
		return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: lock content digest: %w", err)
	}
	stored, err := queries.GetProjectSeedByDigest(ctx, dbsql.GetProjectSeedByDigestParams{
		OwnerUserID: snapshot.OwnerUserID, Digest: snapshot.Digest,
	})
	seed := candidate
	if err == nil {
		storedSnapshot := domain.ProjectSeedSnapshot{
			ID: stored.ID, OwnerUserID: stored.OwnerUserID, RepositoryURL: stored.RepositoryUrl,
			BaseRevision: stored.BaseRevision, Digest: stored.Digest,
			GitBundleDigest:       optionalStringValue(stored.GitBundleDigest),
			TrackedPatchDigest:    optionalStringValue(stored.TrackedPatchDigest),
			UntrackedBundleDigest: optionalStringValue(stored.UntrackedBundleDigest),
			ManifestDigest:        stored.ManifestDigest, CreatedAt: stored.CreatedAt.Time,
		}
		storedInput, marshalErr := json.Marshal(projectSeedInputFromSnapshot(storedSnapshot))
		if marshalErr != nil {
			return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: encode stored input: %w", marshalErr)
		}
		if !sameJSON(storedInput, input) {
			return domain.ProjectSeed{}, ErrIdempotencyConflict
		}
		seed, err = domain.RegisterProjectSeed(storedSnapshot)
		if err != nil {
			return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: restore content address: %w", err)
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		if _, err := queries.InsertProjectSeed(ctx, dbsql.InsertProjectSeedParams{
			ID: snapshot.ID, OwnerUserID: snapshot.OwnerUserID, RepositoryUrl: snapshot.RepositoryURL,
			BaseRevision: snapshot.BaseRevision, Digest: snapshot.Digest,
			GitBundleDigest:       optionalString(snapshot.GitBundleDigest),
			TrackedPatchDigest:    optionalString(snapshot.TrackedPatchDigest),
			UntrackedBundleDigest: optionalString(snapshot.UntrackedBundleDigest),
			ManifestDigest:        snapshot.ManifestDigest, CreatedAt: timestamp(snapshot.CreatedAt),
		}); err != nil {
			return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: insert content: %w", err)
		}
	} else {
		return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: read content address: %w", err)
	}
	if err := queries.InsertProjectSeedRegistration(ctx, dbsql.InsertProjectSeedRegistrationParams{
		OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey,
		Input: input, ProjectSeedID: seed.Snapshot().ID,
	}); err != nil {
		return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: insert idempotency key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ProjectSeed{}, fmt.Errorf("register Project Seed: commit: %w", err)
	}
	return seed, nil
}

func projectSeedInputFromSnapshot(snapshot domain.ProjectSeedSnapshot) projectSeedInput {
	return projectSeedInput{
		RepositoryURL: snapshot.RepositoryURL, BaseRevision: snapshot.BaseRevision, Digest: snapshot.Digest,
		GitBundleDigest: snapshot.GitBundleDigest, TrackedPatchDigest: snapshot.TrackedPatchDigest,
		UntrackedBundleDigest: snapshot.UntrackedBundleDigest, ManifestDigest: snapshot.ManifestDigest,
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
