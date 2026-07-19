package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

func (store *Store) UpdateAutoStopPolicy(ctx context.Context, ownerID string, policy domain.AutoStopPolicy, candidate domain.Operation) (domain.Operation, bool, error) {
	policySnapshot, operation := policy.Snapshot(), candidate.Snapshot()
	if strings.TrimSpace(ownerID) == "" || ownerID != strings.TrimSpace(ownerID) ||
		policySnapshot.EnvironmentID != operation.EnvironmentID || ownerID != operation.RequestedByUserID {
		return domain.Operation{}, false, errors.New("update Auto-stop Policy: canonical owner and matching Policy and Operation are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	if err := lockOperationIdempotencyKey(ctx, tx, ownerID, operation.IdempotencyKey); err != nil {
		return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: lock idempotency key: %w", err)
	}
	existing, err := queries.GetOperationByIdempotencyKey(ctx, dbsql.GetOperationByIdempotencyKeyParams{
		OwnerUserID: ownerID, IdempotencyKey: operation.IdempotencyKey,
	})
	if err == nil {
		if existing.EnvironmentID != operation.EnvironmentID || existing.Type != string(operation.Type) || !sameJSON(existing.Input, operation.Input) {
			return domain.Operation{}, false, ErrIdempotencyConflict
		}
		restored, err := restoreOperation(existing)
		if err != nil {
			return domain.Operation{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: commit replay: %w", err)
		}
		return restored, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: read idempotency key: %w", err)
	}

	var policyID string
	if err := tx.QueryRow(ctx, `
		SELECT p.id
		FROM environments e
		JOIN auto_stop_policies p ON p.environment_id = e.id
		WHERE e.id = $1 AND e.owner_user_id = $2
		FOR UPDATE OF e, p`, policySnapshot.EnvironmentID, ownerID).Scan(&policyID); errors.Is(err, pgx.ErrNoRows) {
		return domain.Operation{}, false, ErrReferenceNotOwned
	} else if err != nil {
		return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: lock owned Environment: %w", err)
	}
	if policyID != policySnapshot.ID {
		return domain.Operation{}, false, ErrReferenceNotOwned
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM operations WHERE environment_id = $1 AND status IN ('queued', 'running')
	)`, policySnapshot.EnvironmentID).Scan(&active); err != nil {
		return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: inspect active Operation: %w", err)
	}
	if active {
		return domain.Operation{}, false, ErrOperationConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auto_stop_policies
		SET mode = $2, grace_period_seconds = $3
		WHERE id = $1`, policySnapshot.ID, string(policySnapshot.Mode), policySnapshot.GracePeriodSeconds); err != nil {
		return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: persist Policy: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key,
			restate_invocation_id, input, created_at, completed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		operation.ID, operation.EnvironmentID, string(operation.Type), string(operation.Status), operation.RequestedByUserID,
		operation.IdempotencyKey, operation.RestateInvocationID, operation.Input, operation.CreatedAt, operation.CompletedAt,
	); err != nil {
		return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: record Operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Operation{}, false, fmt.Errorf("update Auto-stop Policy: commit: %w", err)
	}
	return candidate, true, nil
}
