package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrOperationConflict = errors.New("Environment already has an active Operation")

func (store *Store) ReplayRuntimeOperation(ctx context.Context, candidate domain.Operation) (domain.EnvironmentRuntimeOperation, bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, false, fmt.Errorf("replay Runtime Operation: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, present, err := replayRuntimeOperation(ctx, store.queries.WithTx(tx), candidate)
	if err != nil || !present {
		return domain.EnvironmentRuntimeOperation{}, present, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.EnvironmentRuntimeOperation{}, false, fmt.Errorf("replay Runtime Operation: commit: %w", err)
	}
	return command, true, nil
}

func (store *Store) ReserveRuntimeOperation(ctx context.Context, candidate domain.Operation) (domain.EnvironmentRuntimeOperation, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("reserve Runtime Operation: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	operation := candidate.Snapshot()
	if err := lockOperationIdempotencyKey(ctx, tx, operation.RequestedByUserID, operation.IdempotencyKey); err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("reserve Runtime Operation: lock idempotency key: %w", err)
	}
	if err := rejectActiveConnectionIntentIdempotencyKey(ctx, tx, operation.RequestedByUserID, operation.IdempotencyKey); err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}

	command, present, err := replayRuntimeOperation(ctx, queries, candidate)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	if present {
		if err := tx.Commit(ctx); err != nil {
			return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("reserve Runtime Operation: commit replay: %w", err)
		}
		return command, nil
	}

	command, err = loadRuntimeOperation(ctx, queries, candidate, nil, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvironmentRuntimeOperation{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	if err := queries.InsertRuntimeOperation(ctx, dbsql.InsertRuntimeOperationParams{
		ID: operation.ID, EnvironmentID: operation.EnvironmentID, Type: string(operation.Type), Status: string(operation.Status),
		RequestedByUserID: operation.RequestedByUserID, IdempotencyKey: operation.IdempotencyKey,
		Input: operation.Input, CreatedAt: timestamp(operation.CreatedAt),
	}); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.ConstraintName == "operations_one_active_per_environment_key" {
			return domain.EnvironmentRuntimeOperation{}, ErrOperationConflict
		}
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("reserve Runtime Operation: insert Operation: %w", err)
	}
	if err := queries.InsertRuntimeOperationTarget(ctx, dbsql.InsertRuntimeOperationTargetParams{
		OperationID: operation.ID, EnvironmentID: operation.EnvironmentID, RuntimeID: command.Runtime().Snapshot().ID,
		OperationType: string(operation.Type),
	}); err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("reserve Runtime Operation: insert target: %w", err)
	}
	if err := queries.InsertRuntimeOperationOutbox(ctx, dbsql.InsertRuntimeOperationOutboxParams{
		OperationID: operation.ID, Kind: string(operation.Type), CreatedAt: timestamp(operation.CreatedAt),
	}); err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("reserve Runtime Operation: insert outbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("reserve Runtime Operation: commit: %w", err)
	}
	return command, nil
}

func replayRuntimeOperation(ctx context.Context, queries *dbsql.Queries, candidate domain.Operation) (domain.EnvironmentRuntimeOperation, bool, error) {
	operation := candidate.Snapshot()
	existing, err := queries.GetOperationByIdempotencyKey(ctx, dbsql.GetOperationByIdempotencyKeyParams{
		OwnerUserID: operation.RequestedByUserID, IdempotencyKey: operation.IdempotencyKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvironmentRuntimeOperation{}, false, nil
	}
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, false, fmt.Errorf("replay Runtime Operation: read idempotency key: %w", err)
	}
	if existing.EnvironmentID != operation.EnvironmentID || existing.Type != string(operation.Type) || !sameJSON(existing.Input, operation.Input) {
		return domain.EnvironmentRuntimeOperation{}, false, ErrIdempotencyConflict
	}
	restored, err := restoreOperation(existing)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, false, err
	}
	targetID, err := queries.GetRuntimeOperationTarget(ctx, existing.ID)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, false, fmt.Errorf("replay Runtime Operation: read target: %w", err)
	}
	command, err := loadRuntimeOperation(ctx, queries, restored, &targetID, true)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, false, err
	}
	return command, true, nil
}

func loadRuntimeOperation(ctx context.Context, queries *dbsql.Queries, operation domain.Operation, targetRuntimeID *string, replay bool) (domain.EnvironmentRuntimeOperation, error) {
	snapshot := operation.Snapshot()
	row, err := queries.GetOwnedRuntimeStateForUpdate(ctx, dbsql.GetOwnedRuntimeStateForUpdateParams{
		EnvironmentID: snapshot.EnvironmentID, OwnerUserID: snapshot.RequestedByUserID, RuntimeID: targetRuntimeID,
	})
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	if !row.EnvironmentCreatedAt.Valid || !row.EnvironmentUpdatedAt.Valid || !row.RuntimeCreatedAt.Valid || !row.RuntimeUpdatedAt.Valid {
		return domain.EnvironmentRuntimeOperation{}, errors.New("restore Runtime Operation: database returned invalid timestamps")
	}
	environment, err := domain.RestoreEnvironment(domain.EnvironmentSnapshot{
		ID: row.EnvironmentID, OwnerUserID: row.OwnerUserID, Name: row.Name, Slug: row.Slug,
		Lifecycle: domain.EnvironmentLifecycle(row.Lifecycle), Health: domain.EnvironmentHealth(row.Health),
		Region: row.EnvironmentRegion, AvailabilityZone: row.EnvironmentAvailabilityZone,
		RuntimePreset: row.EnvironmentRuntimePreset, PinnedProfileVersionID: row.PinnedProfileVersionID,
		CapsuleLockID: row.CapsuleLockID, UpgradePolicy: domain.UpgradePolicy(row.UpgradePolicy),
		CurrentRuntimeID: row.CurrentRuntimeID, AutoStopPolicyID: row.AutoStopPolicyID,
		CreatedAt: row.EnvironmentCreatedAt.Time, UpdatedAt: row.EnvironmentUpdatedAt.Time,
		DeletedAt: optionalTime(row.DeletedAt), Version: row.EnvironmentVersion,
	})
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	runtime, err := domain.RestoreRuntime(domain.RuntimeSnapshot{
		ID: row.RuntimeID, EnvironmentID: row.RuntimeEnvironmentID, Sequence: row.Sequence,
		Status: domain.RuntimeStatus(row.RuntimeStatus), RuntimePreset: row.RuntimeRuntimePreset,
		Region: row.RuntimeRegion, AvailabilityZone: row.RuntimeAvailabilityZone, ImageVersion: row.ImageVersion,
		ProviderInstanceRef: row.ProviderInstanceRef, PrivateAddress: row.PrivateAddress, BootID: row.BootID,
		StartedAt: optionalTime(row.StartedAt), StoppedAt: optionalTime(row.StoppedAt), RetiredAt: optionalTime(row.RetiredAt),
		CreatedAt: row.RuntimeCreatedAt.Time, UpdatedAt: row.RuntimeUpdatedAt.Time, Version: row.RuntimeVersion,
	})
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	if replay {
		return domain.RestoreEnvironmentRuntimeOperation(environment, runtime, operation)
	}
	return domain.NewEnvironmentRuntimeOperation(environment, runtime, operation)
}

func restoreOperation(row dbsql.GetOperationByIdempotencyKeyRow) (domain.Operation, error) {
	if !row.CreatedAt.Valid {
		return domain.Operation{}, errors.New("restore Runtime Operation: database returned invalid creation time")
	}
	return domain.RestoreOperation(domain.OperationSnapshot{
		ID: row.ID, EnvironmentID: row.EnvironmentID, Type: domain.OperationType(row.Type), Status: domain.OperationStatus(row.Status),
		RequestedByUserID: row.RequestedByUserID, IdempotencyKey: row.IdempotencyKey,
		RestateInvocationID: row.RestateInvocationID, Input: row.Input,
		CreatedAt: row.CreatedAt.Time, CompletedAt: optionalTime(row.CompletedAt),
	})
}

func optionalTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}
