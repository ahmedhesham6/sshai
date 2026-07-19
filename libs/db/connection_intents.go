package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

// ConnectionIntentRecord is the persisted response identity for one
// authenticated User and Idempotency-Key. Expired records are replaced under
// the shared Operation idempotency lock when the key is used again.
type ConnectionIntentRecord struct {
	OwnerUserID    string
	IdempotencyKey string
	EnvironmentID  string
	OperationID    *string
	IntentID       string
	ExpiresAt      time.Time
}

// CreateOrReplayConnectionIntent serializes a Connection Intent against every
// Operation using the same authenticated User and Idempotency-Key. prepare may
// reserve or join a start Operation; mint is deliberately invoked only after
// the final Environment/active-Operation check, so refused requests do not mint
// an Intent identity.
func (store *Store) CreateOrReplayConnectionIntent(
	ctx context.Context,
	ownerID, idempotencyKey, environmentID string,
	observedAt, expiresAt time.Time,
	prepare func(context.Context) (*string, error),
	mint func() string,
) (ConnectionIntentRecord, error) {
	if !canonicalConnectionIntentIdentity(ownerID) || !canonicalConnectionIntentIdentity(idempotencyKey) ||
		!canonicalConnectionIntentIdentity(environmentID) || observedAt.IsZero() || !expiresAt.After(observedAt) || prepare == nil || mint == nil {
		return ConnectionIntentRecord{}, errors.New("create Connection Intent: canonical identities, expiry, preparation, and minting are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOperationIdempotencyKey(ctx, tx, ownerID, idempotencyKey); err != nil {
		return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: lock idempotency key: %w", err)
	}

	existing, present, err := loadConnectionIntentForUpdate(ctx, tx, ownerID, idempotencyKey)
	if err != nil {
		return ConnectionIntentRecord{}, err
	}
	if present && existing.ExpiresAt.After(observedAt) {
		if existing.EnvironmentID != environmentID {
			return ConnectionIntentRecord{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: commit replay: %w", err)
		}
		return existing, nil
	}
	if present {
		if _, err := tx.Exec(ctx, `
			DELETE FROM connection_intent_idempotency
			WHERE owner_user_id = $1 AND idempotency_key = $2`, ownerID, idempotencyKey); err != nil {
			return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: replace expired record: %w", err)
		}
	}
	var operationExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM operations WHERE requested_by_user_id = $1 AND idempotency_key = $2
	)`, ownerID, idempotencyKey).Scan(&operationExists); err != nil {
		return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: inspect Operation idempotency key: %w", err)
	}
	if operationExists {
		return ConnectionIntentRecord{}, ErrIdempotencyConflict
	}

	operationID, err := prepare(ctx)
	if err != nil {
		return ConnectionIntentRecord{}, err
	}
	operationID, err = lockConnectionIntentEnvironment(ctx, tx, ownerID, environmentID, operationID)
	if err != nil {
		return ConnectionIntentRecord{}, err
	}
	intentID := mint()
	if !canonicalConnectionIntentIdentity(intentID) {
		return ConnectionIntentRecord{}, errors.New("create Connection Intent: minted identity is invalid")
	}
	record := ConnectionIntentRecord{
		OwnerUserID: ownerID, IdempotencyKey: idempotencyKey, EnvironmentID: environmentID,
		OperationID: operationID, IntentID: intentID, ExpiresAt: expiresAt.UTC(),
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO connection_intent_idempotency (
			owner_user_id, idempotency_key, environment_id, operation_id, intent_id, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		record.OwnerUserID, record.IdempotencyKey, record.EnvironmentID, record.OperationID, record.IntentID, record.ExpiresAt); err != nil {
		return ConnectionIntentRecord{}, classifyRepositoryError(fmt.Errorf("create Connection Intent: persist response: %w", err))
	}
	if err := tx.Commit(ctx); err != nil {
		return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: commit: %w", err)
	}
	return record, nil
}

func loadConnectionIntentForUpdate(ctx context.Context, tx pgx.Tx, ownerID, idempotencyKey string) (ConnectionIntentRecord, bool, error) {
	var record ConnectionIntentRecord
	err := tx.QueryRow(ctx, `
		SELECT owner_user_id, idempotency_key, environment_id, operation_id, intent_id, expires_at
		FROM connection_intent_idempotency
		WHERE owner_user_id = $1 AND idempotency_key = $2
		FOR UPDATE`, ownerID, idempotencyKey).Scan(
		&record.OwnerUserID, &record.IdempotencyKey, &record.EnvironmentID, &record.OperationID, &record.IntentID, &record.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectionIntentRecord{}, false, nil
	}
	if err != nil {
		return ConnectionIntentRecord{}, false, fmt.Errorf("create Connection Intent: load replay: %w", err)
	}
	if record.ExpiresAt.IsZero() {
		return ConnectionIntentRecord{}, false, errors.New("create Connection Intent: database returned invalid expiry")
	}
	return record, true, nil
}

func lockConnectionIntentEnvironment(ctx context.Context, tx pgx.Tx, ownerID, environmentID string, preparedOperationID *string) (*string, error) {
	var runtimeStatus *string
	var activeOperationID, activeOperationType *string
	err := tx.QueryRow(ctx, `
		SELECT runtime.status, active.id, active.type
		FROM environments environment
		LEFT JOIN runtimes runtime ON runtime.id = environment.current_runtime_id
		LEFT JOIN operations active
		  ON active.environment_id = environment.id AND active.status IN ('queued', 'running')
		WHERE environment.id = $1 AND environment.owner_user_id = $2
		FOR UPDATE OF environment`, environmentID, ownerID).Scan(&runtimeStatus, &activeOperationID, &activeOperationType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReferenceNotOwned
	}
	if err != nil {
		return nil, fmt.Errorf("create Connection Intent: lock owned Environment: %w", err)
	}
	if activeOperationID != nil {
		if activeOperationType == nil || domain.OperationType(*activeOperationType) != domain.OperationRuntimeStart {
			return nil, ErrOperationConflict
		}
		return activeOperationID, nil
	}
	if runtimeStatus == nil || domain.RuntimeStatus(*runtimeStatus) != domain.RuntimeReady && preparedOperationID == nil {
		return nil, domain.ErrRuntimeCommandState
	}
	if preparedOperationID != nil {
		var validStart bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1
			FROM operations
			WHERE id = $1 AND requested_by_user_id = $2 AND environment_id = $3 AND type = 'runtime.start'
		)`, *preparedOperationID, ownerID, environmentID).Scan(&validStart); err != nil {
			return nil, fmt.Errorf("create Connection Intent: verify prepared start Operation: %w", err)
		}
		if !validStart {
			return nil, errors.New("create Connection Intent: prepared start Operation does not belong to the Environment owner")
		}
	}
	return preparedOperationID, nil
}

func (store *Store) PruneConnectionIntents(ctx context.Context, expiredAt time.Time) (int64, error) {
	if expiredAt.IsZero() {
		return 0, errors.New("prune Connection Intents: expiry boundary is required")
	}
	result, err := store.pool.Exec(ctx, `DELETE FROM connection_intent_idempotency WHERE expires_at <= $1`, expiredAt.UTC())
	if err != nil {
		return 0, fmt.Errorf("prune Connection Intents: %w", err)
	}
	return result.RowsAffected(), nil
}

func canonicalConnectionIntentIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}
