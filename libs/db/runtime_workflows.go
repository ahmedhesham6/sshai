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

func (store *Store) CompleteRuntimeWorkflowOperation(ctx context.Context, operationID string, at time.Time) error {
	return store.finishRuntimeWorkflowOperation(ctx, operationID, domain.OperationSucceeded, "", at)
}

func (store *Store) RecordRuntimeWorkflowFailure(ctx context.Context, operationID, code string, at time.Time) error {
	if code == "" {
		code = "RUNTIME_OPERATION_FAILED"
	}
	return store.finishRuntimeWorkflowOperation(ctx, operationID, domain.OperationFailed, code, at)
}

func (store *Store) finishRuntimeWorkflowOperation(ctx context.Context, operationID string, status domain.OperationStatus, code string, at time.Time) error {
	if operationID == "" || at.IsZero() {
		return permanent(errors.New("finish Runtime workflow Operation: Operation and completion time are required"))
	}
	var result pgtype.Text
	if code != "" {
		result = pgtype.Text{String: code, Valid: true}
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE operations
		SET status = $2, error_code = $3, error_message = $3, completed_at = $4
		WHERE id = $1 AND type IN ('runtime.start', 'runtime.stop', 'runtime.replace')
		  AND status IN ('queued', 'running')`, operationID, string(status), result, at)
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
	var exists bool
	err := store.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1
		FROM environments environment
		JOIN provider_resources resource ON resource.environment_id = environment.id
		WHERE environment.id = $1 AND environment.owner_user_id = $2
		  AND resource.resource_type = 'data_volume'
		  AND resource.provider_id = $3 AND resource.deleted_at IS NULL
	)`, environmentID, ownerID, providerID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("verify Runtime Data Volume: %w", err)
	}
	if !exists {
		return permanent(ErrReferenceNotOwned)
	}
	return nil
}
