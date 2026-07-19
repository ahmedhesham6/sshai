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
	"github.com/jackc/pgx/v5/pgtype"
)

type RuntimeWorkflowState struct {
	OwnerUserID            string
	Runtime                domain.RuntimeSnapshot
	DataVolumeProviderID   string
	ComputeUsageIntervalID string
}

var ErrRuntimeDataUnhealthy = permanent(errors.New("Runtime persistent data is not healthy"))

func (store *Store) LoadRuntimeWorkflowOperation(ctx context.Context, input domain.RuntimeOperationDispatch, invocationID string, at time.Time) (RuntimeWorkflowState, error) {
	if input.OperationID == "" || input.OperationType == "" || input.EnvironmentID == "" || input.RuntimeID == "" || input.OwnerUserID == "" || invocationID == "" || at.IsZero() {
		return RuntimeWorkflowState{}, permanent(errors.New("load Runtime workflow Operation: dispatch, invocation, and start time are required"))
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return RuntimeWorkflowState{}, fmt.Errorf("load Runtime workflow Operation: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	var operationRow dbsql.GetOperationByIdempotencyKeyRow
	err = tx.QueryRow(ctx, `
		SELECT operation.id, operation.environment_id, operation.type, operation.status,
		       operation.requested_by_user_id, operation.idempotency_key,
		       operation.restate_invocation_id, operation.input,
		       operation.created_at, operation.completed_at
		FROM operations operation
		JOIN runtime_operation_targets target ON target.operation_id = operation.id
		WHERE operation.id = $1 AND operation.environment_id = $2
		  AND operation.type = $3 AND operation.requested_by_user_id = $4
		  AND target.runtime_id = $5
		FOR UPDATE OF operation`, input.OperationID, input.EnvironmentID, string(input.OperationType), input.OwnerUserID, input.RuntimeID).Scan(
		&operationRow.ID, &operationRow.EnvironmentID, &operationRow.Type, &operationRow.Status,
		&operationRow.RequestedByUserID, &operationRow.IdempotencyKey, &operationRow.RestateInvocationID,
		&operationRow.Input, &operationRow.CreatedAt, &operationRow.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeWorkflowState{}, permanent(ErrReferenceNotOwned)
	}
	if err != nil {
		return RuntimeWorkflowState{}, fmt.Errorf("load Runtime workflow Operation: lock Operation: %w", err)
	}
	operation, err := restoreOperation(operationRow)
	if err != nil {
		return RuntimeWorkflowState{}, permanent(err)
	}
	operation, err = operation.RecordRestateInvocation(invocationID)
	if err != nil {
		return RuntimeWorkflowState{}, permanent(err)
	}
	operation, err = operation.Start(at)
	if err != nil {
		return RuntimeWorkflowState{}, permanent(err)
	}
	updated, err := tx.Exec(ctx, `
		UPDATE operations
		SET restate_invocation_id = $2, status = 'running'
		WHERE id = $1
		  AND (restate_invocation_id IS NULL OR restate_invocation_id = $2)
		  AND status IN ('queued', 'running')`, input.OperationID, invocationID)
	if err != nil {
		return RuntimeWorkflowState{}, fmt.Errorf("load Runtime workflow Operation: record invocation: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return RuntimeWorkflowState{}, permanent(errors.New("load Runtime workflow Operation: Operation belongs to another invocation"))
	}
	updated, err = tx.Exec(ctx, `
		UPDATE workflow_outbox
		SET started_at = COALESCE(started_at, $2), restate_invocation_id = $3
		WHERE operation_id = $1
		  AND (started_at IS NULL OR restate_invocation_id = $3)`, input.OperationID, at, invocationID)
	if err != nil {
		return RuntimeWorkflowState{}, fmt.Errorf("load Runtime workflow Operation: mark outbox started: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return RuntimeWorkflowState{}, permanent(errors.New("load Runtime workflow Operation: outbox belongs to another invocation"))
	}
	command, err := loadRuntimeOperation(ctx, queries, operation, &input.RuntimeID, true)
	if err != nil {
		return RuntimeWorkflowState{}, fmt.Errorf("load Runtime workflow Operation: restore target: %w", err)
	}
	var dataVolumeProviderID string
	if err := tx.QueryRow(ctx, `
		SELECT resource.provider_id
		FROM provider_resources resource
		WHERE resource.environment_id = $1
		  AND resource.resource_type = 'data_volume'
		  AND resource.deleted_at IS NULL`, input.EnvironmentID).Scan(&dataVolumeProviderID); err != nil {
		return RuntimeWorkflowState{}, fmt.Errorf("load Runtime workflow Operation: load Data Volume: %w", err)
	}
	var usageIntervalID *string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM compute_usage_intervals
		WHERE runtime_id = $1 AND ended_at IS NULL`, input.RuntimeID).Scan(&usageIntervalID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RuntimeWorkflowState{}, fmt.Errorf("load Runtime workflow Operation: load Compute Usage Interval: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RuntimeWorkflowState{}, fmt.Errorf("load Runtime workflow Operation: commit: %w", err)
	}
	state := RuntimeWorkflowState{OwnerUserID: input.OwnerUserID, Runtime: command.Runtime().Snapshot(), DataVolumeProviderID: dataVolumeProviderID}
	if usageIntervalID != nil {
		state.ComputeUsageIntervalID = *usageIntervalID
	}
	return state, nil
}

func (store *Store) PersistRuntimeWorkflowTransition(ctx context.Context, operationID string, expectedVersion int64, next domain.RuntimeSnapshot) error {
	if operationID == "" || expectedVersion < 1 {
		return permanent(errors.New("persist Runtime transition: Operation and expected version are required"))
	}
	if _, err := domain.RestoreRuntime(next); err != nil {
		return permanent(err)
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE runtimes runtime
		SET status = $4, provider_instance_ref = $5, private_address = $6, boot_id = $7,
		    started_at = $8, stopped_at = $9, retired_at = $10,
		    updated_at = $11, version = $12
		FROM runtime_operation_targets target
		WHERE target.operation_id = $1
		  AND target.runtime_id = runtime.id
		  AND runtime.id = $2 AND runtime.environment_id = $3
		  AND runtime.version = $13`,
		operationID, next.ID, next.EnvironmentID, string(next.Status), next.ProviderInstanceRef,
		next.PrivateAddress, next.BootID, next.StartedAt, next.StoppedAt, next.RetiredAt,
		next.UpdatedAt, next.Version, expectedVersion)
	if err != nil {
		return fmt.Errorf("persist Runtime transition: %w", err)
	}
	if result.RowsAffected() != 1 {
		return permanent(domain.ErrStaleRuntimeObservation)
	}
	return nil
}

// PersistRuntimeReplacement atomically retires the Operation target, reserves
// its successor, and moves the Environment's current Runtime pointer. Keeping
// those writes in one transaction is required by the deferred current-Runtime
// foreign key and ensures both Runtime rows remain historical records.
func (store *Store) PersistRuntimeReplacement(ctx context.Context, operationID, ownerUserID string, expectedVersion int64, retired domain.RuntimeSnapshot, reservation domain.RuntimeReservation) (domain.RuntimeSnapshot, error) {
	if operationID == "" || ownerUserID == "" || expectedVersion < 1 {
		return domain.RuntimeSnapshot{}, permanent(errors.New("persist Runtime replacement: Operation, owner, and expected version are required"))
	}
	retiredRuntime, err := domain.RestoreRuntime(retired)
	if err != nil || retired.Status != domain.RuntimeAbsent || retired.RetiredAt == nil || retired.Version != expectedVersion+1 {
		return domain.RuntimeSnapshot{}, permanent(errors.New("persist Runtime replacement: valid immediately-retired Runtime is required"))
	}
	replacementRuntime, err := domain.ReserveRuntime(reservation)
	if err != nil {
		return domain.RuntimeSnapshot{}, permanent(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.RuntimeSnapshot{}, fmt.Errorf("persist Runtime replacement: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var environment domain.EnvironmentSnapshot
	var targetRuntimeID string
	var deletedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `
		SELECT environment.id, environment.owner_user_id, environment.name, environment.slug,
		       environment.lifecycle, environment.health, environment.region, environment.availability_zone,
		       environment.runtime_preset, environment.pinned_profile_version_id, environment.capsule_lock_id,
		       environment.upgrade_policy, environment.current_runtime_id, policy.id,
		       environment.created_at, environment.updated_at, environment.deleted_at, environment.version,
		       target.runtime_id
		FROM operations operation
		JOIN runtime_operation_targets target ON target.operation_id = operation.id
		JOIN environments environment ON environment.id = target.environment_id
		JOIN auto_stop_policies policy ON policy.environment_id = environment.id
		WHERE operation.id = $1 AND operation.requested_by_user_id = $2
		  AND operation.type IN ('runtime.start', 'runtime.replace')
		  AND operation.status = 'running'
		FOR UPDATE OF environment`, operationID, ownerUserID).Scan(
		&environment.ID, &environment.OwnerUserID, &environment.Name, &environment.Slug,
		&environment.Lifecycle, &environment.Health, &environment.Region, &environment.AvailabilityZone,
		&environment.RuntimePreset, &environment.PinnedProfileVersionID, &environment.CapsuleLockID,
		&environment.UpgradePolicy, &environment.CurrentRuntimeID, &environment.AutoStopPolicyID,
		&environment.CreatedAt, &environment.UpdatedAt, &deletedAt, &environment.Version,
		&targetRuntimeID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RuntimeSnapshot{}, permanent(ErrReferenceNotOwned)
	}
	if err != nil {
		return domain.RuntimeSnapshot{}, fmt.Errorf("persist Runtime replacement: lock Environment: %w", err)
	}
	environment.DeletedAt = optionalTime(deletedAt)
	currentEnvironment, err := domain.RestoreEnvironment(environment)
	if err != nil {
		return domain.RuntimeSnapshot{}, permanent(err)
	}
	if targetRuntimeID != retired.ID || reservation.EnvironmentID != environment.ID {
		return domain.RuntimeSnapshot{}, permanent(ErrReferenceNotOwned)
	}
	queries := store.queries.WithTx(tx)
	storedOld, err := loadInitialRuntime(ctx, queries, environment.ID, targetRuntimeID)
	if err != nil {
		return domain.RuntimeSnapshot{}, err
	}
	if environment.CurrentRuntimeID != nil && *environment.CurrentRuntimeID == reservation.ID {
		storedReplacement, loadErr := loadInitialRuntime(ctx, queries, environment.ID, reservation.ID)
		if loadErr != nil {
			return domain.RuntimeSnapshot{}, loadErr
		}
		if storedOld.Snapshot().Status != domain.RuntimeAbsent || storedOld.Snapshot().RetiredAt == nil || !sameRuntimeReservation(storedReplacement.Snapshot(), reservation) {
			return domain.RuntimeSnapshot{}, permanent(ErrInitialRuntimeConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.RuntimeSnapshot{}, fmt.Errorf("persist Runtime replacement: commit replay: %w", err)
		}
		return storedReplacement.Snapshot(), nil
	}
	if environment.CurrentRuntimeID == nil || *environment.CurrentRuntimeID != targetRuntimeID || storedOld.Snapshot().Version != expectedVersion {
		return domain.RuntimeSnapshot{}, permanent(domain.ErrStaleRuntimeObservation)
	}
	updatedEnvironment, err := currentEnvironment.ReplaceRuntime(retiredRuntime, replacementRuntime, reservation.CreatedAt)
	if err != nil {
		return domain.RuntimeSnapshot{}, permanent(err)
	}
	if err := persistRuntimeTransitionTx(ctx, tx, operationID, expectedVersion, retired); err != nil {
		return domain.RuntimeSnapshot{}, err
	}
	replacement := replacementRuntime.Snapshot()
	if err := queries.InsertInitialRuntime(ctx, dbsql.InsertInitialRuntimeParams{
		ID: replacement.ID, EnvironmentID: replacement.EnvironmentID, Sequence: replacement.Sequence,
		Status: string(replacement.Status), RuntimePreset: replacement.RuntimePreset, Region: replacement.Region,
		AvailabilityZone: replacement.AvailabilityZone, ImageVersion: replacement.ImageVersion,
		ProviderInstanceRef: replacement.ProviderInstanceRef, PrivateAddress: replacement.PrivateAddress,
		BootID: replacement.BootID, StartedAt: optionalTimestamp(replacement.StartedAt),
		StoppedAt: optionalTimestamp(replacement.StoppedAt), RetiredAt: optionalTimestamp(replacement.RetiredAt),
		CreatedAt: timestamp(replacement.CreatedAt), UpdatedAt: timestamp(replacement.UpdatedAt), Version: replacement.Version,
	}); err != nil {
		return domain.RuntimeSnapshot{}, classifyRepositoryError(fmt.Errorf("persist Runtime replacement: insert replacement: %w", err))
	}
	nextEnvironment := updatedEnvironment.Snapshot()
	result, err := tx.Exec(ctx, `
		UPDATE environments
		SET current_runtime_id = $2, updated_at = $3, version = $4
		WHERE id = $1 AND current_runtime_id = $5 AND version = $6`,
		nextEnvironment.ID, nextEnvironment.CurrentRuntimeID, nextEnvironment.UpdatedAt,
		nextEnvironment.Version, targetRuntimeID, environment.Version)
	if err != nil {
		return domain.RuntimeSnapshot{}, fmt.Errorf("persist Runtime replacement: switch current Runtime: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.RuntimeSnapshot{}, permanent(domain.ErrStaleRuntimeObservation)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RuntimeSnapshot{}, classifyRepositoryError(fmt.Errorf("persist Runtime replacement: commit: %w", err))
	}
	return replacement, nil
}

func (store *Store) PersistReplacementRuntimeTransition(ctx context.Context, operationID string, expectedVersion int64, next domain.RuntimeSnapshot) error {
	if operationID == "" || expectedVersion < 1 {
		return permanent(errors.New("persist replacement Runtime transition: Operation and expected version are required"))
	}
	if _, err := domain.RestoreRuntime(next); err != nil {
		return permanent(err)
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE runtimes runtime
		SET status = $4, provider_instance_ref = $5, private_address = $6, boot_id = $7,
		    started_at = $8, stopped_at = $9, retired_at = $10,
		    updated_at = $11, version = $12
		FROM runtime_operation_targets target
		JOIN environments environment ON environment.id = target.environment_id
		WHERE target.operation_id = $1 AND target.environment_id = runtime.environment_id
		  AND environment.current_runtime_id = runtime.id
		  AND runtime.id = $2 AND runtime.environment_id = $3 AND runtime.version = $13`,
		operationID, next.ID, next.EnvironmentID, string(next.Status), next.ProviderInstanceRef,
		next.PrivateAddress, next.BootID, next.StartedAt, next.StoppedAt, next.RetiredAt,
		next.UpdatedAt, next.Version, expectedVersion)
	if err != nil {
		return fmt.Errorf("persist replacement Runtime transition: %w", err)
	}
	if result.RowsAffected() != 1 {
		return permanent(domain.ErrStaleRuntimeObservation)
	}
	return nil
}

func persistRuntimeTransitionTx(ctx context.Context, tx pgx.Tx, operationID string, expectedVersion int64, next domain.RuntimeSnapshot) error {
	result, err := tx.Exec(ctx, `
		UPDATE runtimes runtime
		SET status = $4, provider_instance_ref = $5, private_address = $6, boot_id = $7,
		    started_at = $8, stopped_at = $9, retired_at = $10,
		    updated_at = $11, version = $12
		FROM runtime_operation_targets target
		WHERE target.operation_id = $1 AND target.runtime_id = runtime.id
		  AND runtime.id = $2 AND runtime.environment_id = $3 AND runtime.version = $13`,
		operationID, next.ID, next.EnvironmentID, string(next.Status), next.ProviderInstanceRef,
		next.PrivateAddress, next.BootID, next.StartedAt, next.StoppedAt, next.RetiredAt,
		next.UpdatedAt, next.Version, expectedVersion)
	if err != nil {
		return fmt.Errorf("persist retired Runtime transition: %w", err)
	}
	if result.RowsAffected() != 1 {
		return permanent(domain.ErrStaleRuntimeObservation)
	}
	return nil
}

func (store *Store) CompleteRuntimeWorkflowOperation(ctx context.Context, operationID string, at time.Time) error {
	return store.finishRuntimeWorkflowOperation(ctx, operationID, domain.OperationSucceeded, "", "", at)
}

func (store *Store) RecordRuntimeWorkflowFailure(ctx context.Context, operationID, code, message string, at time.Time) error {
	if code == "" {
		code = "RUNTIME_OPERATION_FAILED"
	}
	if message == "" {
		message = code
	}
	return store.finishRuntimeWorkflowOperation(ctx, operationID, domain.OperationFailed, code, message, at)
}

func (store *Store) finishRuntimeWorkflowOperation(ctx context.Context, operationID string, status domain.OperationStatus, code, message string, at time.Time) error {
	if operationID == "" || at.IsZero() {
		return permanent(errors.New("finish Runtime workflow Operation: Operation and completion time are required"))
	}
	var errorCode, errorMessage pgtype.Text
	if code != "" {
		errorCode = pgtype.Text{String: code, Valid: true}
		errorMessage = pgtype.Text{String: message, Valid: true}
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE operations
		SET status = $2, error_code = $3, error_message = $4, completed_at = $5
		WHERE id = $1 AND type IN ('runtime.start', 'runtime.stop', 'runtime.replace')
		  AND status IN ('queued', 'running')`, operationID, string(status), errorCode, errorMessage, at)
	if err != nil {
		return fmt.Errorf("finish Runtime workflow Operation: %w", err)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var currentStatus string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM operations WHERE id = $1`, operationID).Scan(&currentStatus); err != nil {
		return fmt.Errorf("finish Runtime workflow Operation: load replay: %w", err)
	}
	if currentStatus != string(status) {
		return permanent(fmt.Errorf("finish Runtime workflow Operation: status is %q, want %q", currentStatus, status))
	}
	return nil
}

func (store *Store) RecordRuntimeWorkflowStep(ctx context.Context, operationID, stepKey, summary string, at time.Time) error {
	if operationID == "" || stepKey == "" || summary == "" || at.IsZero() {
		return permanent(errors.New("record Runtime workflow step: Operation, step, summary, and time are required"))
	}
	result, err := store.pool.Exec(ctx, `
		INSERT INTO operation_steps (
			id, operation_id, step_key, status, attempt, summary, started_at, completed_at
		) VALUES ($1, $2, $3, 'succeeded', 1, $4, $5, $5)
		ON CONFLICT (operation_id, step_key) DO UPDATE
		SET summary = EXCLUDED.summary
		WHERE operation_steps.summary = EXCLUDED.summary`,
		operationID+":"+stepKey, operationID, stepKey, summary, at)
	if err != nil {
		return fmt.Errorf("record Runtime workflow step: %w", err)
	}
	if result.RowsAffected() != 1 {
		return permanent(ErrIdempotencyConflict)
	}
	return nil
}

func (store *Store) RecordRuntimeStopSnapshotEvidence(ctx context.Context, operationID string, snapshot *domain.AutoStopActivitySnapshot, at time.Time) error {
	if snapshot == nil {
		return permanent(errors.New("record Runtime stop Snapshot: Activity Snapshot is required"))
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return permanent(err)
	}
	return store.RecordRuntimeWorkflowStep(ctx, operationID, "activity-snapshot", string(encoded), at)
}

func (store *Store) RecordRuntimeStopAuditEvidence(ctx context.Context, operationID string, evidence domain.RuntimeStopAuditEvidence, at time.Time) error {
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return permanent(err)
	}
	return store.RecordRuntimeWorkflowStep(ctx, operationID, "auto-stop-audit", string(encoded), at)
}

func (store *Store) VerifyRuntimeDataVolumeOwnership(ctx context.Context, ownerID, environmentID, providerID string) error {
	if ownerID == "" || environmentID == "" || providerID == "" {
		return permanent(errors.New("verify Runtime Data Volume: owner, Environment, and provider identity are required"))
	}
	var owned, healthy bool
	err := store.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1
		FROM environments environment
		JOIN provider_resources resource ON resource.environment_id = environment.id
		WHERE environment.id = $1 AND environment.owner_user_id = $2
		  AND resource.resource_type = 'data_volume'
		  AND resource.provider_id = $3 AND resource.deleted_at IS NULL
	), EXISTS (
		SELECT 1
		FROM environments environment
		JOIN provider_resources resource ON resource.environment_id = environment.id
		WHERE environment.id = $1 AND environment.owner_user_id = $2
		  AND environment.health = 'healthy'
		  AND resource.resource_type = 'data_volume'
		  AND resource.provider_id = $3 AND resource.deleted_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM state_components component
			WHERE component.environment_id = environment.id
			  AND component.durability = 'durable' AND component.health <> 'healthy'
		  )
	)`, environmentID, ownerID, providerID).Scan(&owned, &healthy)
	if err != nil {
		return fmt.Errorf("verify Runtime Data Volume: %w", err)
	}
	if !owned {
		return permanent(ErrReferenceNotOwned)
	}
	if !healthy {
		return ErrRuntimeDataUnhealthy
	}
	return nil
}
