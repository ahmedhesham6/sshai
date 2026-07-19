package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const operationIdempotencyLockNamespace = "user-operation-idempotency"

func lockOperationIdempotencyKey(ctx context.Context, tx pgx.Tx, ownerID, idempotencyKey string) error {
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(
			hashtextextended($1 || chr(31) || $2 || chr(31) || $3, 0)
		)`, operationIdempotencyLockNamespace, ownerID, idempotencyKey); err != nil {
		return fmt.Errorf("lock Operation idempotency key: %w", err)
	}
	return nil
}

func rejectActiveConnectionIntentIdempotencyKey(ctx context.Context, tx pgx.Tx, ownerID, idempotencyKey string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1
		FROM connection_intent_idempotency
		WHERE owner_user_id = $1 AND idempotency_key = $2
		  AND expires_at > statement_timestamp()
	)`, ownerID, idempotencyKey).Scan(&active); err != nil {
		return fmt.Errorf("inspect Connection Intent idempotency key: %w", err)
	}
	if active {
		return ErrIdempotencyConflict
	}
	return nil
}
