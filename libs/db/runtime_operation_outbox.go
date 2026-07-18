package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

func (store *Store) PendingRuntimeOperation(ctx context.Context, operationID string) (domain.RuntimeOperationDispatch, bool, error) {
	row, err := store.queries.GetPendingRuntimeOperation(ctx, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RuntimeOperationDispatch{}, false, nil
	}
	if err != nil {
		return domain.RuntimeOperationDispatch{}, false, fmt.Errorf("read Runtime Operation outbox: %w", err)
	}
	return runtimeOperationDispatch(row.OperationID, row.OperationType, row.EnvironmentID, row.RuntimeID, row.RequestedByUserID, row.StopReason), true, nil
}

func (store *Store) PendingRuntimeOperations(ctx context.Context, limit int) ([]domain.RuntimeOperationDispatch, error) {
	if limit < 1 {
		return nil, errors.New("read Runtime Operation outbox: limit must be positive")
	}
	rows, err := store.queries.ListPendingRuntimeOperations(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("read Runtime Operation outbox: %w", err)
	}
	dispatches := make([]domain.RuntimeOperationDispatch, len(rows))
	for index, row := range rows {
		dispatches[index] = runtimeOperationDispatch(row.OperationID, row.OperationType, row.EnvironmentID, row.RuntimeID, row.RequestedByUserID, row.StopReason)
	}
	return dispatches, nil
}

func runtimeOperationDispatch(operationID, operationType, environmentID, runtimeID, ownerUserID, stopReason string) domain.RuntimeOperationDispatch {
	return domain.RuntimeOperationDispatch{
		OperationID: operationID, OperationType: domain.OperationType(operationType),
		EnvironmentID: environmentID, RuntimeID: runtimeID, OwnerUserID: ownerUserID,
		StopReason: domain.RuntimeStopReason(stopReason),
	}
}
