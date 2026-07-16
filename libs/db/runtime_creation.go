package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

var ErrInitialRuntimeConflict = permanent(errors.New("initial Runtime reservation conflicts with recorded Runtime"))

func (store *Store) ReserveInitialRuntime(ctx context.Context, operationID string, reservation domain.RuntimeReservation) (domain.Runtime, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Runtime{}, fmt.Errorf("reserve initial Runtime: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	creation, err := lockEnvironmentCreation(ctx, queries, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Runtime{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.Runtime{}, fmt.Errorf("reserve initial Runtime: lock Environment creation: %w", err)
	}
	backends, err := queries.ListEnvironmentStateBackendsByOperation(ctx, operationID)
	if err != nil {
		return domain.Runtime{}, fmt.Errorf("reserve initial Runtime: read Environment State: %w", err)
	}
	if len(backends) == 0 {
		return domain.Runtime{}, ErrEnvironmentStateRequired
	}
	if _, err := restoreEnvironmentState(ctx, queries, creation, backends); err != nil {
		return domain.Runtime{}, fmt.Errorf("reserve initial Runtime: %w", err)
	}

	environment := creation.Environment().Snapshot()
	if environment.CurrentRuntimeID != nil {
		runtime, err := loadInitialRuntime(ctx, queries, environment.ID, *environment.CurrentRuntimeID)
		if err != nil {
			return domain.Runtime{}, err
		}
		if !sameRuntimeReservation(runtime.Snapshot(), reservation) {
			return domain.Runtime{}, ErrInitialRuntimeConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Runtime{}, classifyRepositoryError(fmt.Errorf("reserve initial Runtime: commit replay: %w", err))
		}
		return runtime, nil
	}

	updatedCreation, runtime, err := creation.ReserveInitialRuntime(reservation)
	if err != nil {
		return domain.Runtime{}, permanent(fmt.Errorf("reserve initial Runtime: %w", err))
	}
	runtimeSnapshot := runtime.Snapshot()
	if err := queries.InsertInitialRuntime(ctx, dbsql.InsertInitialRuntimeParams{
		ID: runtimeSnapshot.ID, EnvironmentID: runtimeSnapshot.EnvironmentID, Sequence: runtimeSnapshot.Sequence,
		Status: string(runtimeSnapshot.Status), RuntimePreset: runtimeSnapshot.RuntimePreset, Region: runtimeSnapshot.Region,
		AvailabilityZone: runtimeSnapshot.AvailabilityZone, ImageVersion: runtimeSnapshot.ImageVersion,
		ProviderInstanceRef: runtimeSnapshot.ProviderInstanceRef, PrivateAddress: runtimeSnapshot.PrivateAddress,
		BootID: runtimeSnapshot.BootID, StartedAt: optionalTimestamp(runtimeSnapshot.StartedAt),
		StoppedAt: optionalTimestamp(runtimeSnapshot.StoppedAt), RetiredAt: optionalTimestamp(runtimeSnapshot.RetiredAt),
		CreatedAt: timestamp(runtimeSnapshot.CreatedAt), UpdatedAt: timestamp(runtimeSnapshot.UpdatedAt), Version: runtimeSnapshot.Version,
	}); err != nil {
		return domain.Runtime{}, classifyRepositoryError(fmt.Errorf("reserve initial Runtime: insert Runtime: %w", err))
	}
	updatedEnvironment := updatedCreation.Environment().Snapshot()
	updated, err := queries.AttachInitialRuntime(ctx, dbsql.AttachInitialRuntimeParams{
		RuntimeID: updatedEnvironment.CurrentRuntimeID, UpdatedAt: timestamp(updatedEnvironment.UpdatedAt),
		NextVersion: updatedEnvironment.Version, EnvironmentID: updatedEnvironment.ID, CurrentVersion: environment.Version,
	})
	if err != nil {
		return domain.Runtime{}, classifyRepositoryError(fmt.Errorf("reserve initial Runtime: attach Runtime: %w", err))
	}
	if updated != 1 {
		return domain.Runtime{}, ErrInitialRuntimeConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Runtime{}, classifyRepositoryError(fmt.Errorf("reserve initial Runtime: commit: %w", err))
	}
	return runtime, nil
}

func loadInitialRuntime(ctx context.Context, queries *dbsql.Queries, environmentID, runtimeID string) (domain.Runtime, error) {
	row, err := queries.GetInitialRuntimeForUpdate(ctx, dbsql.GetInitialRuntimeForUpdateParams{
		EnvironmentID: environmentID, RuntimeID: runtimeID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Runtime{}, ErrInitialRuntimeConflict
	}
	if err != nil {
		return domain.Runtime{}, fmt.Errorf("reserve initial Runtime: read Runtime: %w", err)
	}
	if !row.CreatedAt.Valid || !row.UpdatedAt.Valid {
		return domain.Runtime{}, permanent(errors.New("reserve initial Runtime: database returned invalid timestamps"))
	}
	runtime, err := domain.RestoreRuntime(domain.RuntimeSnapshot{
		ID: row.ID, EnvironmentID: row.EnvironmentID, Sequence: row.Sequence, Status: domain.RuntimeStatus(row.Status),
		RuntimePreset: row.RuntimePreset, Region: row.Region, AvailabilityZone: row.AvailabilityZone,
		ImageVersion: row.ImageVersion, ProviderInstanceRef: row.ProviderInstanceRef, PrivateAddress: row.PrivateAddress,
		BootID: row.BootID, StartedAt: optionalTime(row.StartedAt), StoppedAt: optionalTime(row.StoppedAt),
		RetiredAt: optionalTime(row.RetiredAt), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, Version: row.Version,
	})
	return runtime, permanent(err)
}

func sameRuntimeReservation(runtime domain.RuntimeSnapshot, reservation domain.RuntimeReservation) bool {
	return runtime.ID == reservation.ID && runtime.EnvironmentID == reservation.EnvironmentID && runtime.Sequence == reservation.Sequence &&
		runtime.RuntimePreset == reservation.RuntimePreset && runtime.Region == reservation.Region &&
		runtime.AvailabilityZone == reservation.AvailabilityZone && runtime.ImageVersion == reservation.ImageVersion &&
		runtime.CreatedAt.Equal(reservation.CreatedAt)
}
