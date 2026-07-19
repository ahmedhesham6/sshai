package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

// PersistEnvironmentCreateRuntimeTransition advances the initial Runtime
// owned by an environment.create Operation. It deliberately has no resource
// compensation behavior; the persistent data volume is outside this method.
func (store *Store) PersistEnvironmentCreateRuntimeTransition(ctx context.Context, operationID string, expectedVersion int64, next domain.RuntimeSnapshot) error {
	if operationID == "" || expectedVersion < 1 || next.ID == "" || next.EnvironmentID == "" {
		return permanent(errors.New("persist Environment create Runtime transition: Operation, Runtime, Environment, and version are required"))
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE runtimes runtime
		SET status = $4, provider_instance_ref = $5, private_address = $6, boot_id = $7,
		    started_at = $8, stopped_at = $9, retired_at = $10,
		    updated_at = $11, version = $12
		FROM environments environment, operations operation
		WHERE operation.id = $1 AND operation.type = 'environment.create'
		  AND operation.environment_id = environment.id
		  AND environment.current_runtime_id = runtime.id
		  AND runtime.id = $2 AND runtime.environment_id = $3
		  AND runtime.version = $13`,
		operationID, next.ID, next.EnvironmentID, string(next.Status), next.ProviderInstanceRef,
		next.PrivateAddress, next.BootID, next.StartedAt, next.StoppedAt, next.RetiredAt,
		next.UpdatedAt, next.Version, expectedVersion)
	if err != nil {
		return fmt.Errorf("persist Environment create Runtime transition: %w", err)
	}
	if result.RowsAffected() != 1 {
		return permanent(domain.ErrStaleRuntimeObservation)
	}
	return nil
}

// FinishEnvironmentCreateOperation records a terminal failed or blocked
// projection. A blocked Operation is the durable requires_input outcome.
func (store *Store) FinishEnvironmentCreateOperation(ctx context.Context, operationID string, status domain.OperationStatus, code, message string, at time.Time) error {
	if operationID == "" || at.IsZero() || code == "" || message == "" {
		return permanent(errors.New("finish Environment create Operation: Operation, outcome, message, and completion time are required"))
	}
	if status != domain.OperationFailed && status != domain.OperationBlocked {
		return permanent(fmt.Errorf("finish Environment create Operation: unsupported status %q", status))
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE operations
		SET status = $2, error_code = $3, error_message = $4, completed_at = $5
		WHERE id = $1 AND type = 'environment.create' AND status IN ('queued', 'running')`,
		operationID, string(status), code, message, at)
	if err != nil {
		return fmt.Errorf("finish Environment create Operation: %w", err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var storedStatus, storedCode, storedMessage string
	if err := store.pool.QueryRow(ctx, `
		SELECT status, error_code, error_message
		FROM operations WHERE id = $1 AND type = 'environment.create'`, operationID).Scan(&storedStatus, &storedCode, &storedMessage); err != nil {
		return fmt.Errorf("finish Environment create Operation: load replay: %w", err)
	}
	if storedStatus != string(status) || storedCode != code || storedMessage != message {
		return permanent(fmt.Errorf("finish Environment create Operation: recorded outcome diverged"))
	}
	return nil
}
