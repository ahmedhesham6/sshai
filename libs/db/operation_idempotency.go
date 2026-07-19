package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const operationIdempotencyLockNamespace = "runtime-operation"

func lockOperationIdempotencyKey(ctx context.Context, tx pgx.Tx, ownerID, idempotencyKey string) error {
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(
			hashtextextended($1 || chr(31) || $2 || chr(31) || $3, 0)
		)`, operationIdempotencyLockNamespace, ownerID, idempotencyKey); err != nil {
		return fmt.Errorf("lock Operation idempotency key: %w", err)
	}
	return nil
}
