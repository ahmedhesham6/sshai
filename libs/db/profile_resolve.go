package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrCapsuleLockConflict = errors.New("Capsule Lock target already has different content")

type profileResolveOperationInput struct {
	EnvironmentID        string  `json:"environmentId"`
	ProfileVersionID     string  `json:"profileVersionId"`
	ProjectCapsuleDigest *string `json:"projectCapsuleDigest,omitempty"`
}

func (store *Store) RecordProfileResolveInvocation(ctx context.Context, operationID, invocationID, environmentID, profileVersionID string, projectCapsuleDigest *string, at time.Time) error {
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(invocationID) == "" || strings.TrimSpace(environmentID) == "" || strings.TrimSpace(profileVersionID) == "" {
		return errors.New("record Profile resolve invocation: identifiers are required")
	}
	input, err := json.Marshal(profileResolveOperationInput{
		EnvironmentID: environmentID, ProfileVersionID: profileVersionID, ProjectCapsuleDigest: cloneDigestPointer(projectCapsuleDigest),
	})
	if err != nil {
		return fmt.Errorf("record Profile resolve invocation: encode input: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record Profile resolve invocation: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	row, err := queries.GetProfileResolveOperation(ctx, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		ownerRow, ownerErr := queries.GetEnvironmentOwner(ctx, environmentID)
		if errors.Is(ownerErr, pgx.ErrNoRows) {
			return ErrReferenceNotOwned
		}
		if ownerErr != nil {
			return fmt.Errorf("record Profile resolve invocation: load Environment owner: %w", ownerErr)
		}
		operation, operationErr := domain.QueueOperation(domain.OperationRequest{
			ID: operationID, EnvironmentID: environmentID, Type: domain.OperationProfileResolve,
			RequestedByUserID: ownerRow, IdempotencyKey: "profile.resolve:" + operationID,
			Input: input, CreatedAt: at.UTC(),
		})
		if operationErr != nil {
			return operationErr
		}
		operation, operationErr = operation.RecordRestateInvocation(invocationID)
		if operationErr != nil {
			return operationErr
		}
		snapshot := operation.Snapshot()
		if err := queries.InsertProfileResolveOperation(ctx, dbsql.InsertProfileResolveOperationParams{
			ID: snapshot.ID, EnvironmentID: snapshot.EnvironmentID, Status: string(snapshot.Status), RequestedByUserID: snapshot.RequestedByUserID,
			IdempotencyKey: snapshot.IdempotencyKey, RestateInvocationID: snapshot.RestateInvocationID, Input: snapshot.Input,
			CreatedAt: timestamp(snapshot.CreatedAt), CompletedAt: optionalTimestamp(snapshot.CompletedAt),
		}); err != nil {
			var pgError *pgconn.PgError
			if errors.As(err, &pgError) && pgError.ConstraintName == "operations_pkey" {
				return fmt.Errorf("record Profile resolve invocation: concurrent Operation creation: %w", err)
			}
			return fmt.Errorf("record Profile resolve invocation: insert Operation: %w", err)
		}
		if err := queries.InsertProfileResolveStep(ctx, dbsql.InsertProfileResolveStepParams{
			ID: operationID + ":resolve", OperationID: operationID, StartedAt: timestamp(at.UTC()),
		}); err != nil {
			return fmt.Errorf("record Profile resolve invocation: insert Operation Step: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("record Profile resolve invocation: load Operation: %w", err)
	} else {
		if row.EnvironmentID != environmentID || !sameJSON(row.Input, input) {
			return ErrIdempotencyConflict
		}
		operation, restoreErr := restoreProfileResolveOperation(row)
		if restoreErr != nil {
			return restoreErr
		}
		operation, restoreErr = operation.RecordRestateInvocation(invocationID)
		if restoreErr != nil {
			return restoreErr
		}
		if _, updateErr := queries.RecordOperationRestateInvocation(ctx, dbsql.RecordOperationRestateInvocationParams{
			RestateInvocationID: operation.Snapshot().RestateInvocationID, OperationID: operationID,
		}); updateErr != nil {
			return fmt.Errorf("record Profile resolve invocation: update Operation: %w", updateErr)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record Profile resolve invocation: commit: %w", err)
	}
	return nil
}

func (store *Store) LoadProfileVersion(ctx context.Context, environmentID, profileVersionID string) (domain.ProfileVersionData, error) {
	row, err := store.queries.GetProfileVersionForEnvironment(ctx, dbsql.GetProfileVersionForEnvironmentParams{
		EnvironmentID: environmentID, ProfileVersionID: profileVersionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProfileVersionData{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.ProfileVersionData{}, fmt.Errorf("load Profile Version for resolve: %w", err)
	}
	version, err := restoreProfileVersion(ctx, store.queries, dbsql.ProfileVersion{
		ID: row.ID, ProfileID: row.ProfileID, ParentVersionID: row.ParentVersionID, Version: row.Version,
		Digest: row.Digest, CreatedAt: row.CreatedAt,
	})
	if err != nil {
		return domain.ProfileVersionData{}, err
	}
	snapshot := version.Snapshot()
	return domain.ProfileVersionData{ID: snapshot.ID, CapsuleRefs: snapshot.CapsuleRefs}, nil
}

func (store *Store) PersistCapsuleLock(ctx context.Context, candidate domain.CapsuleLock) (domain.CapsuleLock, error) {
	snapshot := candidate.Snapshot()
	capsules, err := json.Marshal(snapshot.Capsules)
	if err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: encode Capsules: %w", err)
	}
	components, err := json.Marshal(snapshot.ResolvedComponents)
	if err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: encode Components: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	existing, err := queries.GetCapsuleLockByTarget(ctx, dbsql.GetCapsuleLockByTargetParams{
		EnvironmentID: snapshot.EnvironmentID, ProfileVersionID: snapshot.ProfileVersionID, ProjectCapsuleDigest: snapshot.ProjectCapsuleDigest,
	})
	if err == nil {
		lock, restoreErr := restoreCapsuleLock(existing)
		if restoreErr != nil {
			return domain.CapsuleLock{}, restoreErr
		}
		if lock.Snapshot().Digest != snapshot.Digest {
			return domain.CapsuleLock{}, ErrCapsuleLockConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: commit replay: %w", err)
		}
		return lock, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: load target: %w", err)
	}
	if err := queries.InsertCapsuleLock(ctx, dbsql.InsertCapsuleLockParams{
		ID: snapshot.ID, EnvironmentID: snapshot.EnvironmentID, ProfileVersionID: snapshot.ProfileVersionID,
		ProjectCapsuleDigest: snapshot.ProjectCapsuleDigest, Digest: snapshot.Digest, Capsules: capsules,
		ResolvedComponents: components, CreatedAt: timestamp(snapshot.CreatedAt),
	}); err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "23505" && (pgError.ConstraintName == "capsule_locks_target_key" || pgError.ConstraintName == "capsule_locks_digest_key") {
			return store.loadCapsuleLockAfterConcurrentInsert(ctx, snapshot)
		}
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: commit: %w", err)
	}
	return candidate, nil
}

func (store *Store) loadCapsuleLockAfterConcurrentInsert(ctx context.Context, snapshot domain.CapsuleLockSnapshot) (domain.CapsuleLock, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: begin concurrent replay: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, err := store.queries.WithTx(tx).GetCapsuleLockByTarget(ctx, dbsql.GetCapsuleLockByTargetParams{
		EnvironmentID: snapshot.EnvironmentID, ProfileVersionID: snapshot.ProfileVersionID, ProjectCapsuleDigest: snapshot.ProjectCapsuleDigest,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CapsuleLock{}, ErrCapsuleLockConflict
	}
	if err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: load concurrent winner: %w", err)
	}
	lock, err := restoreCapsuleLock(existing)
	if err != nil {
		return domain.CapsuleLock{}, err
	}
	if lock.Snapshot().Digest != snapshot.Digest {
		return domain.CapsuleLock{}, ErrCapsuleLockConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Capsule Lock: commit concurrent replay: %w", err)
	}
	return lock, nil
}

func (store *Store) CompleteProfileResolve(ctx context.Context, operationID string, at time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("complete Profile resolve: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	row, err := queries.GetProfileResolveOperation(ctx, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReferenceNotOwned
	}
	if err != nil {
		return fmt.Errorf("complete Profile resolve: load Operation: %w", err)
	}
	if row.Status == string(domain.OperationSucceeded) {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("complete Profile resolve: commit replay: %w", err)
		}
		return nil
	}
	if _, err := queries.CompleteProfileResolveOperation(ctx, dbsql.CompleteProfileResolveOperationParams{OperationID: operationID, CompletedAt: timestamp(at.UTC())}); err != nil {
		return fmt.Errorf("complete Profile resolve: update Operation: %w", err)
	}
	if _, err := queries.CompleteProfileResolveStep(ctx, dbsql.CompleteProfileResolveStepParams{OperationID: operationID, CompletedAt: timestamp(at.UTC())}); err != nil {
		return fmt.Errorf("complete Profile resolve: update Operation Step: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("complete Profile resolve: commit: %w", err)
	}
	return nil
}

func restoreProfileResolveOperation(row dbsql.GetProfileResolveOperationRow) (domain.Operation, error) {
	if !row.CreatedAt.Valid {
		return domain.Operation{}, errors.New("restore Profile resolve Operation: database returned invalid creation time")
	}
	return domain.RestoreOperation(domain.OperationSnapshot{
		ID: row.ID, EnvironmentID: row.EnvironmentID, Type: domain.OperationType(row.Type), Status: domain.OperationStatus(row.Status),
		RequestedByUserID: row.RequestedByUserID, IdempotencyKey: row.IdempotencyKey, RestateInvocationID: row.RestateInvocationID,
		Input: row.Input, CreatedAt: row.CreatedAt.Time, CompletedAt: optionalTime(row.CompletedAt),
	})
}

func restoreCapsuleLock(row dbsql.CapsuleLock) (domain.CapsuleLock, error) {
	if !row.CreatedAt.Valid {
		return domain.CapsuleLock{}, errors.New("restore Capsule Lock: database returned invalid creation time")
	}
	var capsules []domain.LockedCapsule
	if err := json.Unmarshal(row.Capsules, &capsules); err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("restore Capsule Lock: decode Capsules: %w", err)
	}
	components := make(map[string]domain.ResolvedComponent)
	if err := json.Unmarshal(row.ResolvedComponents, &components); err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("restore Capsule Lock: decode Components: %w", err)
	}
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: row.ID, EnvironmentID: row.EnvironmentID, ProfileVersionID: row.ProfileVersionID,
		ProjectCapsuleDigest: row.ProjectCapsuleDigest, Capsules: capsules, ResolvedComponents: components,
		Digest: row.Digest, CreatedAt: row.CreatedAt.Time.UTC(),
	})
	if err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("restore Capsule Lock: %w", err)
	}
	return lock, nil
}

func cloneDigestPointer(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
