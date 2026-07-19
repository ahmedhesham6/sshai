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
)

func (store *Store) PendingRuntimeOperation(ctx context.Context, operationID string) (domain.RuntimeOperationDispatch, bool, error) {
	row, err := store.queries.GetPendingRuntimeOperation(ctx, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RuntimeOperationDispatch{}, false, nil
	}
	if err != nil {
		return domain.RuntimeOperationDispatch{}, false, fmt.Errorf("read Runtime Operation outbox: %w", err)
	}
	dispatch, err := runtimeOperationDispatch(row.OperationID, row.OperationType, row.EnvironmentID, row.RuntimeID, row.RequestedByUserID, row.StopReason, row.OperationInput)
	if err != nil {
		return domain.RuntimeOperationDispatch{}, false, err
	}
	return dispatch, true, nil
}

func (store *Store) DeferRuntimeOperationDispatch(ctx context.Context, operationID string, attemptedAt time.Time) error {
	if operationID == "" || attemptedAt.IsZero() {
		return errors.New("defer Runtime Operation dispatch: Operation and attempt time are required")
	}
	if _, err := store.queries.DeferRuntimeOperationDispatch(ctx, dbsql.DeferRuntimeOperationDispatchParams{
		OperationID: operationID, AttemptedAt: timestamp(attemptedAt),
	}); err != nil {
		return fmt.Errorf("defer Runtime Operation dispatch: %w", err)
	}
	return nil
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
		dispatch, err := runtimeOperationDispatch(row.OperationID, row.OperationType, row.EnvironmentID, row.RuntimeID, row.RequestedByUserID, row.StopReason, row.OperationInput)
		if err != nil {
			return nil, err
		}
		dispatches[index] = dispatch
	}
	return dispatches, nil
}

func runtimeOperationDispatch(operationID, operationType, environmentID, runtimeID, ownerUserID, stopReason string, operationInput []byte) (domain.RuntimeOperationDispatch, error) {
	var input struct {
		Audit *domain.RuntimeStopAuditEvidence `json:"audit"`
	}
	if err := json.Unmarshal(operationInput, &input); err != nil {
		return domain.RuntimeOperationDispatch{}, fmt.Errorf("read Runtime Operation outbox: decode input: %w", err)
	}
	return domain.RuntimeOperationDispatch{
		OperationID: operationID, OperationType: domain.OperationType(operationType),
		EnvironmentID: environmentID, RuntimeID: runtimeID, OwnerUserID: ownerUserID,
		StopReason: domain.RuntimeStopReason(stopReason), StopAudit: domain.CloneRuntimeStopAuditEvidence(input.Audit),
	}, nil
}
