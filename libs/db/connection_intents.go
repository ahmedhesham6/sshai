package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrConnectionIntentNotFound  = errors.New("Connection Intent not found")
	ErrConnectionIntentExpired   = errors.New("Connection Intent expired")
	ErrConnectionIntentUsed      = errors.New("Connection Intent already used")
	ErrConnectionIntentInvariant = errors.New("Connection Intent persistence invariant violated")
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
	UsedAt         *time.Time
}

// ConsumeConnectionIntent atomically spends one unexpired Connection Intent
// for the WorkOS subject and Environment presented at the regional proxy.
// Missing and foreign records are deliberately indistinguishable.
func (store *Store) ConsumeConnectionIntent(ctx context.Context, workOSUserID, intentID, environmentID string, observedAt time.Time) (ConnectionIntentRecord, error) {
	if !canonicalConnectionIntentIdentity(workOSUserID) || !canonicalConnectionIntentIdentity(intentID) ||
		!canonicalConnectionIntentIdentity(environmentID) || observedAt.IsZero() {
		return ConnectionIntentRecord{}, errors.New("consume Connection Intent: canonical identities and observation time are required")
	}
	var record ConnectionIntentRecord
	var expiresAt, usedAt pgtype.Timestamptz
	err := store.pool.QueryRow(ctx, `
		UPDATE connection_intent_idempotency intent
		SET used_at = GREATEST(intent.created_at, $4)
		FROM users owner
		WHERE intent.owner_user_id = owner.id
		  AND owner.workos_user_id = $1
		  AND intent.intent_id = $2
		  AND intent.environment_id = $3
		  AND intent.expires_at > $4
		  AND intent.used_at IS NULL
		RETURNING intent.owner_user_id, intent.idempotency_key, intent.environment_id,
		          intent.operation_id, intent.intent_id, intent.expires_at, intent.used_at`,
		workOSUserID, intentID, environmentID, observedAt.UTC(),
	).Scan(
		&record.OwnerUserID, &record.IdempotencyKey, &record.EnvironmentID,
		&record.OperationID, &record.IntentID, &expiresAt, &usedAt,
	)
	if err == nil {
		if !expiresAt.Valid || !usedAt.Valid {
			return ConnectionIntentRecord{}, errors.New("consume Connection Intent: database returned invalid timestamps")
		}
		record.ExpiresAt = expiresAt.Time
		record.UsedAt = timePointer(usedAt.Time)
		return record, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ConnectionIntentRecord{}, classifyConnectionIntentConsumeError(err)
	}
	if _, validationErr := store.ValidateConnectionIntent(ctx, workOSUserID, intentID, environmentID, observedAt); validationErr != nil {
		return ConnectionIntentRecord{}, validationErr
	}
	return ConnectionIntentRecord{}, ErrConnectionIntentInvariant
}

// ValidateConnectionIntent performs the owner, Environment, expiry, and use
// checks without spending the Intent. It is used only to reject bad WebSocket
// handshakes before upgrade; ConsumeConnectionIntent remains authoritative.
func (store *Store) ValidateConnectionIntent(ctx context.Context, workOSUserID, intentID, environmentID string, observedAt time.Time) (ConnectionIntentRecord, error) {
	if !canonicalConnectionIntentIdentity(workOSUserID) || !canonicalConnectionIntentIdentity(intentID) ||
		!canonicalConnectionIntentIdentity(environmentID) || observedAt.IsZero() {
		return ConnectionIntentRecord{}, errors.New("validate Connection Intent: canonical identities and observation time are required")
	}
	var record ConnectionIntentRecord
	var expiresAt, usedAt pgtype.Timestamptz
	err := store.pool.QueryRow(ctx, `
		SELECT intent.owner_user_id, intent.idempotency_key, intent.environment_id,
		       intent.operation_id, intent.intent_id, intent.expires_at, intent.used_at
		FROM connection_intent_idempotency intent
		JOIN users owner ON owner.id = intent.owner_user_id
		WHERE owner.workos_user_id = $1
		  AND intent.intent_id = $2
		  AND intent.environment_id = $3`,
		workOSUserID, intentID, environmentID,
	).Scan(
		&record.OwnerUserID, &record.IdempotencyKey, &record.EnvironmentID,
		&record.OperationID, &record.IntentID, &expiresAt, &usedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectionIntentRecord{}, ErrConnectionIntentNotFound
	}
	if err != nil {
		return ConnectionIntentRecord{}, fmt.Errorf("validate Connection Intent: %w", err)
	}
	if usedAt.Valid {
		return ConnectionIntentRecord{}, ErrConnectionIntentUsed
	}
	if !expiresAt.Valid || !expiresAt.Time.After(observedAt) {
		return ConnectionIntentRecord{}, ErrConnectionIntentExpired
	}
	record.ExpiresAt = expiresAt.Time
	return record, nil
}

func classifyConnectionIntentConsumeError(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23514" {
		return fmt.Errorf("%w: %v", ErrConnectionIntentInvariant, err)
	}
	return fmt.Errorf("consume Connection Intent: %w", err)
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
	existing, present, err := store.replayConnectionIntent(ctx, ownerID, idempotencyKey, environmentID, observedAt)
	if err != nil || present {
		return existing, err
	}
	operationID, err := prepare(ctx)
	if err != nil {
		return ConnectionIntentRecord{}, err
	}
	record, err := store.persistPreparedConnectionIntent(
		ctx, ownerID, idempotencyKey, environmentID, observedAt, expiresAt, operationID, mint,
	)
	var insertRace *connectionIntentInsertRaceError
	if !errors.As(err, &insertRace) {
		return record, err
	}
	existing, present, replayErr := store.replayConnectionIntent(ctx, ownerID, idempotencyKey, environmentID, observedAt)
	if replayErr != nil {
		return ConnectionIntentRecord{}, replayErr
	}
	if present {
		return existing, nil
	}
	return ConnectionIntentRecord{}, classifyRepositoryError(insertRace.cause)
}

func (store *Store) replayConnectionIntent(ctx context.Context, ownerID, idempotencyKey, environmentID string, observedAt time.Time) (ConnectionIntentRecord, bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ConnectionIntentRecord{}, false, fmt.Errorf("create Connection Intent: begin replay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, replay, _, err := lockLoadValidateConnectionIntentReplay(ctx, tx, ownerID, idempotencyKey, environmentID, observedAt)
	if err != nil {
		return ConnectionIntentRecord{}, false, err
	}
	if replay {
		if err := tx.Commit(ctx); err != nil {
			return ConnectionIntentRecord{}, false, fmt.Errorf("create Connection Intent: commit replay: %w", err)
		}
		return existing, true, nil
	}
	if err := rejectExistingOperationIdempotencyKey(ctx, tx, ownerID, idempotencyKey); err != nil {
		return ConnectionIntentRecord{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConnectionIntentRecord{}, false, fmt.Errorf("create Connection Intent: commit replay miss: %w", err)
	}
	return ConnectionIntentRecord{}, false, nil
}

func (store *Store) persistPreparedConnectionIntent(
	ctx context.Context,
	ownerID, idempotencyKey, environmentID string,
	observedAt, expiresAt time.Time,
	preparedOperationID *string,
	mint func() string,
) (ConnectionIntentRecord, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: begin persist transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, replay, present, err := lockLoadValidateConnectionIntentReplay(ctx, tx, ownerID, idempotencyKey, environmentID, observedAt)
	if err != nil {
		return ConnectionIntentRecord{}, err
	}
	if replay {
		if err := tx.Commit(ctx); err != nil {
			return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: commit concurrent replay: %w", err)
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
	if err := rejectExistingOperationIdempotencyKey(ctx, tx, ownerID, idempotencyKey); err != nil {
		return ConnectionIntentRecord{}, err
	}
	preparedOperationID, err = lockConnectionIntentEnvironment(ctx, tx, ownerID, environmentID, preparedOperationID)
	if err != nil {
		return ConnectionIntentRecord{}, err
	}
	intentID := mint()
	if !canonicalConnectionIntentIdentity(intentID) {
		return ConnectionIntentRecord{}, errors.New("create Connection Intent: minted identity is invalid")
	}
	record := ConnectionIntentRecord{
		OwnerUserID: ownerID, IdempotencyKey: idempotencyKey, EnvironmentID: environmentID,
		OperationID: preparedOperationID, IntentID: intentID, ExpiresAt: expiresAt.UTC(),
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO connection_intent_idempotency (
			owner_user_id, idempotency_key, environment_id, operation_id, intent_id, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		record.OwnerUserID, record.IdempotencyKey, record.EnvironmentID, record.OperationID, record.IntentID, record.ExpiresAt); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return ConnectionIntentRecord{}, &connectionIntentInsertRaceError{cause: fmt.Errorf("create Connection Intent: persist response: %w", err)}
		}
		return ConnectionIntentRecord{}, classifyRepositoryError(fmt.Errorf("create Connection Intent: persist response: %w", err))
	}
	if err := tx.Commit(ctx); err != nil {
		return ConnectionIntentRecord{}, fmt.Errorf("create Connection Intent: commit: %w", err)
	}
	return record, nil
}

func lockLoadValidateConnectionIntentReplay(
	ctx context.Context,
	tx pgx.Tx,
	ownerID, idempotencyKey, environmentID string,
	observedAt time.Time,
) (ConnectionIntentRecord, bool, bool, error) {
	if err := lockOperationIdempotencyKey(ctx, tx, ownerID, idempotencyKey); err != nil {
		return ConnectionIntentRecord{}, false, false, fmt.Errorf("create Connection Intent: lock idempotency key: %w", err)
	}
	existing, present, err := loadConnectionIntentForUpdate(ctx, tx, ownerID, idempotencyKey)
	if err != nil || !present || !existing.ExpiresAt.After(observedAt) {
		return existing, false, present, err
	}
	if existing.EnvironmentID != environmentID {
		return ConnectionIntentRecord{}, false, present, ErrIdempotencyConflict
	}
	return existing, true, present, nil
}

type connectionIntentInsertRaceError struct{ cause error }

func (err *connectionIntentInsertRaceError) Error() string { return err.cause.Error() }
func (err *connectionIntentInsertRaceError) Unwrap() error { return err.cause }

func rejectExistingOperationIdempotencyKey(ctx context.Context, tx pgx.Tx, ownerID, idempotencyKey string) error {
	var operationExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM operations WHERE requested_by_user_id = $1 AND idempotency_key = $2
	)`, ownerID, idempotencyKey).Scan(&operationExists); err != nil {
		return fmt.Errorf("create Connection Intent: inspect Operation idempotency key: %w", err)
	}
	if operationExists {
		return ErrIdempotencyConflict
	}
	return nil
}

func loadConnectionIntentForUpdate(ctx context.Context, tx pgx.Tx, ownerID, idempotencyKey string) (ConnectionIntentRecord, bool, error) {
	var record ConnectionIntentRecord
	var usedAt pgtype.Timestamptz
	err := tx.QueryRow(ctx, `
		SELECT owner_user_id, idempotency_key, environment_id, operation_id, intent_id, expires_at, used_at
		FROM connection_intent_idempotency
		WHERE owner_user_id = $1 AND idempotency_key = $2
		FOR UPDATE`, ownerID, idempotencyKey).Scan(
		&record.OwnerUserID, &record.IdempotencyKey, &record.EnvironmentID, &record.OperationID, &record.IntentID, &record.ExpiresAt, &usedAt,
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
	if usedAt.Valid {
		record.UsedAt = timePointer(usedAt.Time)
	}
	return record, true, nil
}

func lockConnectionIntentEnvironment(ctx context.Context, tx pgx.Tx, ownerID, environmentID string, preparedOperationID *string) (*string, error) {
	var preparedRuntimeID string
	if preparedOperationID != nil {
		err := tx.QueryRow(ctx, `
			SELECT target.runtime_id
			FROM operations operation
			JOIN runtime_operation_targets target ON target.operation_id = operation.id
			WHERE operation.id = $1
			  AND operation.requested_by_user_id = $2
			  AND operation.environment_id = $3
			  AND operation.type = 'runtime.start'
			  AND operation.status IN ('queued', 'running')
			FOR UPDATE OF operation`, *preparedOperationID, ownerID, environmentID).Scan(&preparedRuntimeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrOperationConflict
		}
		if err != nil {
			return nil, fmt.Errorf("create Connection Intent: verify prepared start Operation: %w", err)
		}
	}
	var currentRuntimeID *string
	var activeOperationID, activeOperationType *string
	err := tx.QueryRow(ctx, `
		SELECT environment.current_runtime_id, active.id, active.type
		FROM environments environment
		LEFT JOIN operations active
		  ON active.environment_id = environment.id AND active.status IN ('queued', 'running')
		WHERE environment.id = $1 AND environment.owner_user_id = $2
		FOR UPDATE OF environment`, environmentID, ownerID).Scan(&currentRuntimeID, &activeOperationID, &activeOperationType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReferenceNotOwned
	}
	if err != nil {
		return nil, fmt.Errorf("create Connection Intent: lock owned Environment: %w", err)
	}
	var runtimeStatus *string
	if currentRuntimeID != nil {
		var status string
		if err := tx.QueryRow(ctx, `
			SELECT status
			FROM runtimes
			WHERE id = $1 AND environment_id = $2
			FOR UPDATE`, *currentRuntimeID, environmentID).Scan(&status); err != nil {
			return nil, fmt.Errorf("create Connection Intent: lock current Runtime: %w", err)
		}
		runtimeStatus = &status
	}
	if preparedOperationID != nil && (currentRuntimeID == nil || preparedRuntimeID != *currentRuntimeID) {
		return nil, ErrOperationConflict
	}
	if activeOperationID != nil {
		if preparedOperationID == nil || activeOperationType == nil || domain.OperationType(*activeOperationType) != domain.OperationRuntimeStart {
			return nil, ErrOperationConflict
		}
		if preparedOperationID != nil && *activeOperationID != *preparedOperationID {
			return nil, ErrOperationConflict
		}
		return activeOperationID, nil
	}
	if runtimeStatus == nil || domain.RuntimeStatus(*runtimeStatus) != domain.RuntimeReady && preparedOperationID == nil {
		return nil, domain.ErrRuntimeCommandState
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

func timePointer(value time.Time) *time.Time {
	result := value
	return &result
}
