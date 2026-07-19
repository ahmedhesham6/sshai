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
	"github.com/jackc/pgx/v5/pgtype"
)

type EnvironmentCapsuleApplyState struct {
	PreviousLock     *domain.CapsuleLockSnapshot
	Materializations []EnvironmentMaterialization
	UpgradePolicy    domain.UpgradePolicy
}

func (store *Store) LoadEnvironmentCapsuleApplyState(ctx context.Context, environmentID string) (EnvironmentCapsuleApplyState, error) {
	pin, err := store.GetEnvironmentPin(ctx, environmentID)
	if err != nil {
		return EnvironmentCapsuleApplyState{}, err
	}
	state := EnvironmentCapsuleApplyState{UpgradePolicy: pin.UpgradePolicy}
	if pin.CapsuleLockID != nil {
		row, err := store.queries.GetCapsuleLockForEnvironment(ctx, dbsql.GetCapsuleLockForEnvironmentParams{LockID: *pin.CapsuleLockID, EnvironmentID: environmentID})
		if err != nil {
			return EnvironmentCapsuleApplyState{}, fmt.Errorf("load Profile apply state: load current Capsule Lock: %w", err)
		}
		lock, err := restoreCapsuleLock(row)
		if err != nil {
			return EnvironmentCapsuleApplyState{}, err
		}
		snapshot := lock.Snapshot()
		state.PreviousLock = &snapshot
	}
	state.Materializations, err = store.ListEnvironmentMaterializations(ctx, environmentID)
	if err != nil {
		return EnvironmentCapsuleApplyState{}, fmt.Errorf("load Profile apply state: %w", err)
	}
	return state, nil
}

type CompleteProfileApplyInput struct {
	OperationID      string
	PreviousLockID   *string
	CapsuleLock      domain.CapsuleLock
	UpgradePolicy    domain.UpgradePolicy
	Materializations []EnvironmentMaterialization
	CompletedAt      time.Time
}

func (store *Store) CompleteProfileApply(ctx context.Context, input CompleteProfileApplyInput) error {
	lock := input.CapsuleLock.Snapshot()
	if strings.TrimSpace(input.OperationID) == "" || input.CompletedAt.IsZero() || lock.EnvironmentID == "" {
		return permanent(errors.New("complete Profile apply: Operation, Lock, and completion time are required"))
	}
	policy := input.UpgradePolicy
	if policy == "" {
		policy = domain.UpgradeManual
	}
	if !policy.Valid() {
		return permanent(fmt.Errorf("complete Profile apply: invalid upgrade policy %q", policy))
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("complete Profile apply: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var operationStatus, environmentID string
	var operationInput []byte
	var currentLockID *string
	if err := tx.QueryRow(ctx, `
		SELECT operation.status, operation.input, environment.id, environment.capsule_lock_id
		FROM operations operation
		JOIN runtime_operation_targets target ON target.operation_id = operation.id
		JOIN environments environment ON environment.id = target.environment_id
		WHERE operation.id = $1 AND operation.type = 'profile.apply'
		FOR UPDATE OF operation, environment`, input.OperationID).Scan(&operationStatus, &operationInput, &environmentID, &currentLockID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return permanent(ErrReferenceNotOwned)
		}
		return fmt.Errorf("complete Profile apply: lock state: %w", err)
	}
	if operationStatus == string(domain.OperationSucceeded) {
		if currentLockID == nil || *currentLockID != lock.ID {
			return permanent(ErrIdempotencyConflict)
		}
		return tx.Commit(ctx)
	}
	if operationStatus != string(domain.OperationRunning) || environmentID != lock.EnvironmentID || !sameOptionalString(currentLockID, input.PreviousLockID) {
		return permanent(domain.ErrStaleRuntimeObservation)
	}
	var request struct {
		ProfileVersionID string `json:"profileVersionId"`
	}
	if err := json.Unmarshal(operationInput, &request); err != nil || request.ProfileVersionID != lock.ProfileVersionID {
		return permanent(errors.New("complete Profile apply: Capsule Lock differs from persisted request"))
	}
	queries := store.queries.WithTx(tx)
	persisted, err := persistCapsuleLockForEnvironmentState(ctx, queries, input.CapsuleLock)
	if err != nil {
		return err
	}
	persistedSnapshot := persisted.Snapshot()
	if err := validateEnvironmentMaterializationsAgainstLock(input.Materializations, environmentID, persistedSnapshot); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `
		UPDATE environments
		SET pinned_profile_version_id = $2, capsule_lock_id = $3, upgrade_policy = $4,
		    updated_at = $5, version = version + 1
		WHERE id = $1 AND capsule_lock_id IS NOT DISTINCT FROM $6`, environmentID, persistedSnapshot.ProfileVersionID,
		persistedSnapshot.ID, string(policy), input.CompletedAt, input.PreviousLockID)
	if err != nil {
		return fmt.Errorf("complete Profile apply: pin Capsule Lock: %w", err)
	}
	if result.RowsAffected() != 1 {
		return permanent(domain.ErrStaleRuntimeObservation)
	}
	if _, err := queries.DeleteEnvironmentMaterializations(ctx, environmentID); err != nil {
		return fmt.Errorf("complete Profile apply: replace Materializations: %w", err)
	}
	for _, record := range input.Materializations {
		record.EnvironmentID, record.LockID = environmentID, persistedSnapshot.ID
		if err := upsertEnvironmentMaterialization(ctx, queries, record); err != nil {
			return err
		}
	}
	result, err = tx.Exec(ctx, `
		UPDATE operations
		SET status = 'succeeded', error_code = NULL, error_message = NULL, completed_at = $2
		WHERE id = $1 AND type = 'profile.apply' AND status = 'running'`, input.OperationID, input.CompletedAt)
	if err != nil {
		return fmt.Errorf("complete Profile apply: finish Operation: %w", err)
	}
	if result.RowsAffected() != 1 {
		return permanent(domain.ErrStaleRuntimeObservation)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("complete Profile apply: commit: %w", err)
	}
	return nil
}

func (store *Store) RecordProfileApplyFailure(ctx context.Context, operationID, code, message string, at time.Time) error {
	if code == "" {
		code = "PROFILE_APPLY_FAILED"
	}
	if message == "" {
		message = code
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE operations
		SET status = 'failed', error_code = $2, error_message = $3, completed_at = $4
		WHERE id = $1 AND type = 'profile.apply' AND status IN ('queued', 'running')`,
		operationID, pgtype.Text{String: code, Valid: true}, pgtype.Text{String: message, Valid: true}, at)
	if err != nil {
		return fmt.Errorf("record Profile apply failure: %w", err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var status string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM operations WHERE id = $1 AND type = 'profile.apply'`, operationID).Scan(&status); err != nil {
		return fmt.Errorf("record Profile apply failure: load replay: %w", err)
	}
	if status != string(domain.OperationFailed) {
		return permanent(fmt.Errorf("record Profile apply failure: status is %q", status))
	}
	return nil
}
