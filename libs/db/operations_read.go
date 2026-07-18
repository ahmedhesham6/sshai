package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

// OperationStepProjection is the read-only projection of a single Operation
// Step row. It is deliberately not modeled as a domain aggregate: Operation
// Steps are pure progress reporting, with no invariants of their own beyond
// what the database already enforces.
type OperationStepProjection struct {
	StepKey string
	Status  string
	Summary string
}

// OperationDetail is the read-only projection an owner-scoped Operation query
// resolves to: the Operation aggregate alongside its ordered Steps.
type OperationDetail struct {
	Operation domain.Operation
	Steps     []OperationStepProjection
}

// GetOwnedOperation loads a single Operation requested by ownerID, alongside
// its Steps. An absent or foreign Operation reports ErrReferenceNotOwned.
func (store *Store) GetOwnedOperation(ctx context.Context, ownerID, operationID string) (OperationDetail, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(operationID) == "" {
		return OperationDetail{}, errors.New("get owned Operation: canonical owner and Operation IDs are required")
	}
	row, err := store.queries.GetOwnedOperation(ctx, dbsql.GetOwnedOperationParams{OperationID: operationID, OwnerUserID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationDetail{}, ErrReferenceNotOwned
	}
	if err != nil {
		return OperationDetail{}, fmt.Errorf("get owned Operation: %w", err)
	}
	if !row.CreatedAt.Valid {
		return OperationDetail{}, errors.New("restore Operation: database returned invalid creation time")
	}
	operation, err := domain.RestoreOperation(domain.OperationSnapshot{
		ID: row.ID, EnvironmentID: row.EnvironmentID, Type: domain.OperationType(row.Type), Status: domain.OperationStatus(row.Status),
		RequestedByUserID: row.RequestedByUserID, IdempotencyKey: row.IdempotencyKey, RestateInvocationID: row.RestateInvocationID,
		Input: row.Input, CreatedAt: row.CreatedAt.Time, CompletedAt: optionalTime(row.CompletedAt),
	})
	if err != nil {
		return OperationDetail{}, fmt.Errorf("restore Operation: %w", err)
	}
	stepRows, err := store.queries.ListOperationSteps(ctx, operationID)
	if err != nil {
		return OperationDetail{}, fmt.Errorf("get owned Operation: load Steps: %w", err)
	}
	steps := make([]OperationStepProjection, len(stepRows))
	for index, stepRow := range stepRows {
		steps[index] = OperationStepProjection{StepKey: stepRow.StepKey, Status: stepRow.Status, Summary: stepRow.Summary}
	}
	return OperationDetail{Operation: operation, Steps: steps}, nil
}

// EnvironmentEvent is the read-only projection of a single Environment event:
// an Operation reaching or advancing toward a lifecycle milestone. Today
// every event maps 1:1 onto an Operation record; there is no dedicated event
// table, so the Summary is synthesized from the Operation's type and status
// rather than stored verbatim.
type EnvironmentEvent struct {
	ID            string
	EnvironmentID string
	OperationID   *string
	Type          string
	Summary       string
	CreatedAt     time.Time
}

// ListOwnedEnvironmentEvents loads the Operation timeline for a single
// Environment owned by ownerID, ordered by creation time then ID. An absent
// or foreign Environment reports ErrReferenceNotOwned.
func (store *Store) ListOwnedEnvironmentEvents(ctx context.Context, ownerID, environmentID string) ([]EnvironmentEvent, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(environmentID) == "" {
		return nil, errors.New("list owned Environment events: canonical owner and Environment IDs are required")
	}
	owner, err := store.queries.GetEnvironmentOwner(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReferenceNotOwned
	}
	if err != nil {
		return nil, fmt.Errorf("list owned Environment events: check ownership: %w", err)
	}
	if owner != ownerID {
		return nil, ErrReferenceNotOwned
	}
	rows, err := store.queries.ListOwnedEnvironmentOperations(ctx, dbsql.ListOwnedEnvironmentOperationsParams{
		EnvironmentID: environmentID, OwnerUserID: ownerID,
	})
	if err != nil {
		return nil, fmt.Errorf("list owned Environment events: %w", err)
	}
	events := make([]EnvironmentEvent, len(rows))
	for index, row := range rows {
		if !row.CreatedAt.Valid {
			return nil, errors.New("list owned Environment events: database returned invalid creation time")
		}
		operationID := row.ID
		events[index] = EnvironmentEvent{
			ID: row.ID, EnvironmentID: row.EnvironmentID, OperationID: &operationID,
			Type: row.Type, Summary: fmt.Sprintf("%s %s", row.Type, row.Status), CreatedAt: row.CreatedAt.Time,
		}
	}
	return events, nil
}
