package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrIdempotencyConflict = permanent(errors.New("idempotency key reused with different input"))
	ErrReferenceNotOwned   = permanent(errors.New("referenced resource is absent or not owned"))
)

func (store *Store) ReserveEnvironmentCreation(ctx context.Context, candidate domain.EnvironmentCreation) (domain.EnvironmentCreation, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("reserve Environment creation: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	environment := candidate.Environment().Snapshot()
	operation := candidate.Operation().Snapshot()
	ownerID, idempotencyKey := environment.OwnerUserID, operation.IdempotencyKey
	if _, err := queries.LockEnvironmentCreation(ctx, dbsql.LockEnvironmentCreationParams{
		OwnerUserID: &ownerID, IdempotencyKey: &idempotencyKey,
	}); err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("reserve Environment creation: lock idempotency key: %w", err)
	}

	existing, err := queries.GetEnvironmentCreationByKey(ctx, dbsql.GetEnvironmentCreationByKeyParams{
		OwnerUserID: ownerID, IdempotencyKey: idempotencyKey,
	})
	if err == nil {
		if !sameJSON(existing.OperationInput, operation.Input) {
			return domain.EnvironmentCreation{}, ErrIdempotencyConflict
		}
		creation, err := restoreEnvironmentCreation(ctx, queries, existing)
		if err != nil {
			return domain.EnvironmentCreation{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.EnvironmentCreation{}, fmt.Errorf("reserve Environment creation: commit replay: %w", err)
		}
		return creation, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvironmentCreation{}, fmt.Errorf("reserve Environment creation: find idempotency key: %w", err)
	}

	if err := insertEnvironmentCreation(ctx, queries, candidate); err != nil {
		return domain.EnvironmentCreation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("reserve Environment creation: commit: %w", err)
	}
	return candidate, nil
}

func (store *Store) CompleteEnvironmentCreation(ctx context.Context, operationID string, at time.Time) (domain.EnvironmentCreation, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("complete Environment creation: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	creation, err := lockEnvironmentCreation(ctx, queries, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvironmentCreation{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("complete Environment creation: lock Environment creation: %w", err)
	}
	if creation.Operation().Snapshot().Status == domain.OperationSucceeded {
		if err := tx.Commit(ctx); err != nil {
			return domain.EnvironmentCreation{}, fmt.Errorf("complete Environment creation: commit replay: %w", err)
		}
		return creation, nil
	}
	backends, err := queries.ListEnvironmentStateBackendsByOperation(ctx, operationID)
	if err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("complete Environment creation: read Environment State: %w", err)
	}
	if len(backends) == 0 {
		return domain.EnvironmentCreation{}, ErrEnvironmentStateRequired
	}
	if _, err := restoreEnvironmentState(ctx, queries, creation, backends); err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("complete Environment creation: %w", err)
	}
	completed, err := creation.Complete(at)
	if err != nil {
		return domain.EnvironmentCreation{}, permanent(err)
	}

	before, after := creation.Environment().Snapshot(), completed.Environment().Snapshot()
	updated, err := queries.UpdateCreatedEnvironment(ctx, dbsql.UpdateCreatedEnvironmentParams{
		Lifecycle: string(after.Lifecycle), Health: string(after.Health), UpdatedAt: timestamp(after.UpdatedAt),
		NextVersion: after.Version, EnvironmentID: after.ID, CurrentVersion: before.Version,
	})
	if err != nil {
		return domain.EnvironmentCreation{}, classifyRepositoryError(fmt.Errorf("complete Environment creation: update Environment: %w", err))
	}
	if updated != 1 {
		return domain.EnvironmentCreation{}, permanent(errors.New("complete Environment creation: concurrent Environment update"))
	}
	operationSnapshot := completed.Operation().Snapshot()
	updated, err = queries.CompleteCreatedEnvironmentOperation(ctx, dbsql.CompleteCreatedEnvironmentOperationParams{
		Status: string(operationSnapshot.Status), CompletedAt: optionalTimestamp(operationSnapshot.CompletedAt), OperationID: operationSnapshot.ID,
	})
	if err != nil {
		return domain.EnvironmentCreation{}, classifyRepositoryError(fmt.Errorf("complete Environment creation: update Operation: %w", err))
	}
	if updated != 1 {
		return domain.EnvironmentCreation{}, permanent(errors.New("complete Environment creation: concurrent Operation update"))
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.EnvironmentCreation{}, classifyRepositoryError(fmt.Errorf("complete Environment creation: commit: %w", err))
	}
	return completed, nil
}

func insertEnvironmentCreation(ctx context.Context, queries *dbsql.Queries, creation domain.EnvironmentCreation) error {
	environment := creation.Environment().Snapshot()
	policy := creation.Policy().Snapshot()
	operation := creation.Operation().Snapshot()
	inserted, err := queries.InsertEnvironment(ctx, dbsql.InsertEnvironmentParams{
		ID: environment.ID, OwnerUserID: environment.OwnerUserID, Name: environment.Name, Slug: environment.Slug,
		Lifecycle: string(environment.Lifecycle), Health: string(environment.Health), Region: environment.Region,
		AvailabilityZone: environment.AvailabilityZone, RuntimePreset: environment.RuntimePreset,
		PinnedProfileVersionID: environment.PinnedProfileVersionID, CurrentRuntimeID: environment.CurrentRuntimeID,
		CreatedAt: timestamp(environment.CreatedAt), UpdatedAt: timestamp(environment.UpdatedAt),
		DeletedAt: optionalTimestamp(environment.DeletedAt), Version: environment.Version,
	})
	if err != nil {
		return fmt.Errorf("reserve Environment creation: insert Environment: %w", err)
	}
	if inserted != 1 {
		return ErrReferenceNotOwned
	}
	if err := queries.InsertAutoStopPolicy(ctx, dbsql.InsertAutoStopPolicyParams{
		ID: policy.ID, EnvironmentID: policy.EnvironmentID, Mode: string(policy.Mode), GracePeriodSeconds: int32(policy.GracePeriodSeconds),
	}); err != nil {
		return fmt.Errorf("reserve Environment creation: insert Auto-stop Policy: %w", err)
	}
	seedEnvironmentID := environment.ID
	assigned, err := queries.AssignProjectSeed(ctx, dbsql.AssignProjectSeedParams{
		EnvironmentID: &seedEnvironmentID, ProjectSeedID: creation.ProjectSeedID(), OwnerUserID: environment.OwnerUserID,
	})
	if err != nil {
		return fmt.Errorf("reserve Environment creation: assign Project Seed: %w", err)
	}
	if assigned != 1 {
		return ErrReferenceNotOwned
	}
	sshKeyIDs := creation.SSHKeyIDs()
	if len(sshKeyIDs) == 0 {
		return ErrReferenceNotOwned
	}
	assigned, err = queries.AssignEnvironmentSSHKeys(ctx, dbsql.AssignEnvironmentSSHKeysParams{
		EnvironmentID: environment.ID, OwnerUserID: environment.OwnerUserID, SshKeyIds: sshKeyIDs,
	})
	if err != nil {
		return fmt.Errorf("reserve Environment creation: assign SSH Keys: %w", err)
	}
	if assigned != int64(len(sshKeyIDs)) {
		return ErrReferenceNotOwned
	}
	if err := queries.InsertOperation(ctx, dbsql.InsertOperationParams{
		ID: operation.ID, EnvironmentID: operation.EnvironmentID, Type: string(operation.Type), Status: string(operation.Status),
		RequestedByUserID: operation.RequestedByUserID, IdempotencyKey: operation.IdempotencyKey,
		RestateInvocationID: operation.RestateInvocationID, Input: operation.Input,
		CreatedAt: timestamp(operation.CreatedAt), CompletedAt: optionalTimestamp(operation.CompletedAt),
	}); err != nil {
		return fmt.Errorf("reserve Environment creation: insert Operation: %w", err)
	}
	if err := queries.InsertEnvironmentCreateOutbox(ctx, dbsql.InsertEnvironmentCreateOutboxParams{
		OperationID: operation.ID, CreatedAt: timestamp(operation.CreatedAt),
	}); err != nil {
		return fmt.Errorf("reserve Environment creation: insert workflow outbox: %w", err)
	}
	return nil
}

func lockEnvironmentCreation(ctx context.Context, queries *dbsql.Queries, operationID string) (domain.EnvironmentCreation, error) {
	key, err := queries.GetOperationCreationKeyForUpdate(ctx, operationID)
	if err != nil {
		return domain.EnvironmentCreation{}, err
	}
	row, err := queries.GetEnvironmentCreationByKey(ctx, dbsql.GetEnvironmentCreationByKeyParams{
		OwnerUserID: key.RequestedByUserID, IdempotencyKey: key.IdempotencyKey,
	})
	if err != nil {
		return domain.EnvironmentCreation{}, err
	}
	return restoreEnvironmentCreation(ctx, queries, row)
}

func (store *Store) PendingEnvironmentCreate(ctx context.Context, operationID string) (domain.EnvironmentCreateDispatch, bool, error) {
	row, err := store.queries.GetPendingEnvironmentCreate(ctx, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvironmentCreateDispatch{}, false, nil
	}
	if err != nil {
		return domain.EnvironmentCreateDispatch{}, false, fmt.Errorf("read Environment create outbox: %w", err)
	}
	return environmentCreateDispatch(row.OperationID, row.EnvironmentID, row.Region, row.AvailabilityZone, row.RuntimePreset), true, nil
}

func (store *Store) PendingEnvironmentCreates(ctx context.Context, limit int) ([]domain.EnvironmentCreateDispatch, error) {
	if limit < 1 {
		return nil, errors.New("read Environment create outbox: limit must be positive")
	}
	rows, err := store.queries.ListPendingEnvironmentCreates(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("read Environment create outbox: %w", err)
	}
	result := make([]domain.EnvironmentCreateDispatch, len(rows))
	for index, row := range rows {
		result[index] = environmentCreateDispatch(row.OperationID, row.EnvironmentID, row.Region, row.AvailabilityZone, row.RuntimePreset)
	}
	return result, nil
}

func (store *Store) RecordEnvironmentCreateInvocation(ctx context.Context, operationID, invocationID string, at time.Time) (domain.EnvironmentCreation, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("record Environment create invocation: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	creation, err := lockEnvironmentCreation(ctx, queries, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvironmentCreation{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("record Environment create invocation: lock Environment creation: %w", err)
	}
	creation, err = creation.RecordRestateInvocation(invocationID)
	if err != nil {
		return domain.EnvironmentCreation{}, permanent(err)
	}
	invocation := creation.Operation().Snapshot().RestateInvocationID
	updated, err := queries.RecordOperationRestateInvocation(ctx, dbsql.RecordOperationRestateInvocationParams{
		RestateInvocationID: invocation, OperationID: operationID,
	})
	if err != nil {
		return domain.EnvironmentCreation{}, classifyRepositoryError(fmt.Errorf("record Environment create invocation: update Operation: %w", err))
	}
	if updated != 1 {
		return domain.EnvironmentCreation{}, permanent(errors.New("record Environment create invocation: Operation belongs to another invocation"))
	}
	updated, err = queries.MarkEnvironmentCreateOutboxStarted(ctx, dbsql.MarkEnvironmentCreateOutboxStartedParams{
		StartedAt: timestamp(at), RestateInvocationID: invocation, OperationID: operationID,
	})
	if err != nil {
		return domain.EnvironmentCreation{}, classifyRepositoryError(fmt.Errorf("record Environment create invocation: update outbox: %w", err))
	}
	if updated != 1 {
		return domain.EnvironmentCreation{}, permanent(errors.New("record Environment create invocation: outbox belongs to another invocation"))
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.EnvironmentCreation{}, classifyRepositoryError(fmt.Errorf("record Environment create invocation: commit: %w", err))
	}
	return creation, nil
}

func environmentCreateDispatch(operationID, environmentID, region, availabilityZone, runtimePreset string) domain.EnvironmentCreateDispatch {
	return domain.EnvironmentCreateDispatch{
		OperationID: operationID, EnvironmentID: environmentID, Region: region, AvailabilityZone: availabilityZone,
		RuntimePreset: runtimePreset,
	}
}

func restoreEnvironmentCreation(ctx context.Context, queries *dbsql.Queries, row dbsql.GetEnvironmentCreationByKeyRow) (domain.EnvironmentCreation, error) {
	if !row.EnvironmentCreatedAt.Valid || !row.EnvironmentUpdatedAt.Valid || !row.OperationCreatedAt.Valid {
		return domain.EnvironmentCreation{}, permanent(errors.New("restore Environment creation: database returned invalid timestamps"))
	}
	var deletedAt, completedAt *time.Time
	if row.DeletedAt.Valid {
		deletedAt = &row.DeletedAt.Time
	}
	if row.OperationCompletedAt.Valid {
		completedAt = &row.OperationCompletedAt.Time
	}
	environment, err := domain.RestoreEnvironment(domain.EnvironmentSnapshot{
		ID: row.EnvironmentID, OwnerUserID: row.OwnerUserID, Name: row.Name, Slug: row.Slug,
		Lifecycle: domain.EnvironmentLifecycle(row.Lifecycle), Health: domain.EnvironmentHealth(row.Health),
		Region: row.Region, AvailabilityZone: row.AvailabilityZone, RuntimePreset: row.RuntimePreset,
		PinnedProfileVersionID: row.PinnedProfileVersionID, CurrentRuntimeID: row.CurrentRuntimeID,
		AutoStopPolicyID: row.PolicyID, CreatedAt: row.EnvironmentCreatedAt.Time,
		UpdatedAt: row.EnvironmentUpdatedAt.Time, DeletedAt: deletedAt, Version: row.Version,
	})
	if err != nil {
		return domain.EnvironmentCreation{}, permanent(err)
	}
	policy, err := domain.NewAutoStopPolicy(row.PolicyID, row.EnvironmentID, domain.AutoStopMode(row.PolicyMode), int(row.GracePeriodSeconds))
	if err != nil {
		return domain.EnvironmentCreation{}, permanent(err)
	}
	operation, err := domain.RestoreOperation(domain.OperationSnapshot{
		ID: row.OperationID, EnvironmentID: row.EnvironmentID, Type: domain.OperationType(row.OperationType),
		Status: domain.OperationStatus(row.OperationStatus), RequestedByUserID: row.OwnerUserID,
		IdempotencyKey: row.IdempotencyKey, RestateInvocationID: row.RestateInvocationID,
		Input: row.OperationInput, CreatedAt: row.OperationCreatedAt.Time, CompletedAt: completedAt,
	})
	if err != nil {
		return domain.EnvironmentCreation{}, permanent(err)
	}
	sshKeyIDs, err := queries.ListEnvironmentSSHKeyIDs(ctx, row.EnvironmentID)
	if err != nil {
		return domain.EnvironmentCreation{}, fmt.Errorf("restore Environment creation: list SSH Keys: %w", err)
	}
	creation, err := domain.NewEnvironmentCreation(environment, policy, operation, row.ProjectSeedID, sshKeyIDs)
	if err != nil {
		return domain.EnvironmentCreation{}, permanent(fmt.Errorf("restore Environment creation: %w", err))
	}
	return creation, nil
}

func sameJSON(first, second []byte) bool {
	firstValue, firstOK := comparableJSON(first)
	secondValue, secondOK := comparableJSON(second)
	if !firstOK || !secondOK {
		return false
	}
	return reflect.DeepEqual(firstValue, secondValue)
}

func comparableJSON(input []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return normalizeComparableJSON(value)
}

type comparableJSONNumber string

func normalizeComparableJSON(value any) (any, bool) {
	switch value := value.(type) {
	case nil, bool, string:
		return value, true
	case json.Number:
		rational, ok := new(big.Rat).SetString(string(value))
		if !ok {
			return nil, false
		}
		return comparableJSONNumber(rational.RatString()), true
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			normalized, ok := normalizeComparableJSON(item)
			if !ok {
				return nil, false
			}
			result[index] = normalized
		}
		return result, true
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			normalized, ok := normalizeComparableJSON(item)
			if !ok {
				return nil, false
			}
			result[key] = normalized
		}
		return result, true
	default:
		return nil, false
	}
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func optionalTimestamp(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return timestamp(*value)
}
