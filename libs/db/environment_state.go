package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrEnvironmentStateConflict = permanent(errors.New("Environment State inventory conflicts with recorded Provider Resource"))
	ErrEnvironmentStateRequired = permanent(errors.New("Environment State inventory is required before activation"))
)

func (store *Store) InventoryEnvironmentState(ctx context.Context, operationID string, reservation domain.EnvironmentStateReservation) (domain.EnvironmentState, error) {
	if operationID == "" || operationID != strings.TrimSpace(operationID) {
		return domain.EnvironmentState{}, permanent(errors.New("inventory Environment State: canonical Operation identity is required"))
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.EnvironmentState{}, fmt.Errorf("inventory Environment State: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	creation, err := lockEnvironmentCreation(ctx, queries, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvironmentState{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.EnvironmentState{}, fmt.Errorf("inventory Environment State: lock Environment creation: %w", err)
	}
	backends, err := queries.ListEnvironmentStateBackendsByOperation(ctx, operationID)
	if err != nil {
		return domain.EnvironmentState{}, fmt.Errorf("inventory Environment State: read Provider Resources: %w", err)
	}
	if len(backends) != 0 {
		state, err := restoreEnvironmentState(ctx, queries, creation, backends)
		if err != nil {
			return domain.EnvironmentState{}, err
		}
		backend := state.Backend()
		if reservation.Provider != backend.Provider || reservation.ProviderID != backend.ProviderID || !sameJSON(reservation.Metadata, backend.Metadata) {
			return domain.EnvironmentState{}, ErrEnvironmentStateConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.EnvironmentState{}, fmt.Errorf("inventory Environment State: commit replay: %w", err)
		}
		return state, nil
	}

	state, err := domain.ReserveEnvironmentState(creation.Environment(), creation.Operation(), reservation)
	if err != nil {
		return domain.EnvironmentState{}, permanent(fmt.Errorf("inventory Environment State: %w", err))
	}
	backend := state.Backend()
	if err := queries.InsertEnvironmentStateBackend(ctx, dbsql.InsertEnvironmentStateBackendParams{
		ID: backend.ID, EnvironmentID: backend.EnvironmentID, OperationID: backend.OperationID,
		Provider: backend.Provider, Region: backend.Region, ProviderID: backend.ProviderID,
		Metadata: backend.Metadata, CreatedAt: timestamp(backend.CreatedAt), DeletedAt: optionalTimestamp(backend.DeletedAt),
	}); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.ConstraintName == "provider_resources_provider_identity_key" {
			return domain.EnvironmentState{}, ErrEnvironmentStateConflict
		}
		return domain.EnvironmentState{}, classifyRepositoryError(fmt.Errorf("inventory Environment State: insert Provider Resource: %w", err))
	}
	for _, component := range state.Components() {
		if err := queries.InsertEnvironmentStateComponent(ctx, dbsql.InsertEnvironmentStateComponentParams{
			ID: component.ID, EnvironmentID: component.EnvironmentID, Kind: string(component.Kind),
			Durability: string(component.Durability), MountPath: component.MountPath,
			BackendResourceID: component.BackendResourceID, Health: string(component.Health),
			ObservedDigest: component.ObservedDigest, CreatedAt: timestamp(component.CreatedAt), UpdatedAt: timestamp(component.UpdatedAt),
		}); err != nil {
			return domain.EnvironmentState{}, classifyRepositoryError(fmt.Errorf("inventory Environment State: insert State Component: %w", err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.EnvironmentState{}, classifyRepositoryError(fmt.Errorf("inventory Environment State: commit: %w", err))
	}
	return state, nil
}

func restoreEnvironmentState(ctx context.Context, queries *dbsql.Queries, creation domain.EnvironmentCreation, backends []dbsql.ListEnvironmentStateBackendsByOperationRow) (domain.EnvironmentState, error) {
	if len(backends) != 1 || !backends[0].CreatedAt.Valid {
		return domain.EnvironmentState{}, ErrEnvironmentStateConflict
	}
	backendRow := backends[0]
	components, err := queries.ListEnvironmentStateComponents(ctx, backendRow.EnvironmentID)
	if err != nil {
		return domain.EnvironmentState{}, fmt.Errorf("restore Environment State: read State Components: %w", err)
	}
	snapshots := make([]domain.StateComponentSnapshot, len(components))
	for index, component := range components {
		if !component.CreatedAt.Valid || !component.UpdatedAt.Valid {
			return domain.EnvironmentState{}, permanent(errors.New("restore Environment State: database returned invalid timestamps"))
		}
		snapshots[index] = domain.StateComponentSnapshot{
			ID: component.ID, EnvironmentID: component.EnvironmentID, Kind: domain.StateComponentKind(component.Kind),
			Durability: domain.DurabilityClass(component.Durability), MountPath: component.MountPath,
			BackendResourceID: component.BackendResourceID, Health: domain.EnvironmentHealth(component.Health),
			ObservedDigest: component.ObservedDigest, CreatedAt: component.CreatedAt.Time, UpdatedAt: component.UpdatedAt.Time,
		}
	}
	state, err := domain.RestoreEnvironmentState(creation.Environment(), creation.Operation(), snapshots, domain.DataVolumeResourceSnapshot{
		ID: backendRow.ID, EnvironmentID: backendRow.EnvironmentID, OperationID: backendRow.OperationID,
		Provider: backendRow.Provider, Region: backendRow.Region, ProviderID: backendRow.ProviderID,
		Metadata: backendRow.Metadata, CreatedAt: backendRow.CreatedAt.Time, DeletedAt: optionalTime(backendRow.DeletedAt),
	})
	return state, permanent(err)
}
