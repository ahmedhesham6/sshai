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
	if _, err := domain.RestoreRuntime(next); err != nil {
		return permanent(fmt.Errorf("persist Environment create Runtime transition: validate next Runtime: %w", err))
	}
	if next.Version != expectedVersion+1 {
		return permanent(fmt.Errorf("persist Environment create Runtime transition: next version %d does not follow expected version %d", next.Version, expectedVersion))
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE runtimes runtime
		SET status = $4, provider_instance_ref = $5, private_address = $6, boot_id = $7,
		    started_at = $8, stopped_at = $9, retired_at = $10,
		    updated_at = $11, version = $12
		FROM environments environment, operations operation
		WHERE operation.id = $1 AND operation.type = 'environment.create'
		  AND operation.status IN ('queued', 'running')
		  AND operation.environment_id = environment.id
		  AND environment.current_runtime_id = runtime.id
		  AND runtime.id = $2 AND runtime.environment_id = $3
		  AND runtime.version = $13
		  AND runtime.sequence = $14 AND runtime.runtime_preset = $15
		  AND runtime.region = $16 AND runtime.availability_zone = $17
		  AND runtime.image_version = $18 AND runtime.created_at = $19`,
		operationID, next.ID, next.EnvironmentID, string(next.Status), next.ProviderInstanceRef,
		next.PrivateAddress, next.BootID, next.StartedAt, next.StoppedAt, next.RetiredAt,
		next.UpdatedAt, next.Version, expectedVersion, next.Sequence, next.RuntimePreset,
		next.Region, next.AvailabilityZone, next.ImageVersion, next.CreatedAt)
	if err != nil {
		return fmt.Errorf("persist Environment create Runtime transition: %w", err)
	}
	if result.RowsAffected() != 1 {
		var replayed bool
		if err := store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM runtimes runtime
				JOIN environments environment ON environment.current_runtime_id = runtime.id
				JOIN operations operation ON operation.environment_id = environment.id
				WHERE operation.id = $1 AND operation.type = 'environment.create'
				  AND operation.status IN ('queued', 'running')
				  AND runtime.id = $2 AND runtime.environment_id = $3
				  AND runtime.status = $4
				  AND runtime.provider_instance_ref IS NOT DISTINCT FROM $5::text
				  AND runtime.private_address IS NOT DISTINCT FROM $6::text
				  AND runtime.boot_id IS NOT DISTINCT FROM $7::text
				  AND runtime.started_at IS NOT DISTINCT FROM $8::timestamptz
				  AND runtime.stopped_at IS NOT DISTINCT FROM $9::timestamptz
				  AND runtime.retired_at IS NOT DISTINCT FROM $10::timestamptz
				  AND runtime.updated_at = $11 AND runtime.version = $12
				  AND runtime.sequence = $13 AND runtime.runtime_preset = $14
				  AND runtime.region = $15 AND runtime.availability_zone = $16
				  AND runtime.image_version = $17 AND runtime.created_at = $18
			)`,
			operationID, next.ID, next.EnvironmentID, string(next.Status), next.ProviderInstanceRef,
			next.PrivateAddress, next.BootID, next.StartedAt, next.StoppedAt, next.RetiredAt,
			next.UpdatedAt, next.Version, next.Sequence, next.RuntimePreset,
			next.Region, next.AvailabilityZone, next.ImageVersion, next.CreatedAt,
		).Scan(&replayed); err != nil {
			return fmt.Errorf("persist Environment create Runtime transition: load replay: %w", err)
		}
		if !replayed {
			return permanent(domain.ErrStaleRuntimeObservation)
		}
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
