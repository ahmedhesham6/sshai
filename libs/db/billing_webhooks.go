package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/jackc/pgx/v5"
)

var ErrPolarWebhookConflict = errors.New("Polar webhook event ID reused with different payload")

type ApplyPolarProjectionResult struct {
	Applied           bool
	CreditTransaction *billing.CreditTransaction
}

type SubscriptionProjection struct {
	UserID              string
	PolarSubscriptionID string
	PolarCustomerID     string
	Status              string
	CurrentPeriodStart  time.Time
	CurrentPeriodEnd    time.Time
	CancelAtPeriodEnd   bool
	CanceledAt          *time.Time
	ExternalEventID     string
	ObservedAt          time.Time
}

type PolarCustomerProjection struct {
	UserID          string
	PolarCustomerID string
	ExternalEventID string
	ObservedAt      time.Time
}

func (store *Store) ApplyPolarProjection(
	ctx context.Context,
	projection billing.PolarProjection,
	payloadSHA256 [sha256.Size]byte,
	receivedAt time.Time,
) (ApplyPolarProjectionResult, error) {
	event, err := polarProjectionEvent(projection)
	if err != nil {
		return ApplyPolarProjectionResult{}, err
	}
	if receivedAt.IsZero() {
		return ApplyPolarProjectionResult{}, errors.New("apply Polar projection: valid receive time is required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ApplyPolarProjectionResult{}, fmt.Errorf("apply Polar projection: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	if _, err := queries.LockPolarWebhookReceipt(ctx, event.externalEventID); err != nil {
		return ApplyPolarProjectionResult{}, fmt.Errorf("apply Polar projection: lock external event: %w", err)
	}
	receipt, err := queries.GetPolarWebhookReceipt(ctx, event.externalEventID)
	if err == nil {
		if !bytes.Equal(receipt.PayloadSha256, payloadSHA256[:]) || receipt.EventType != event.eventType ||
			!receipt.OccurredAt.Valid || !receipt.OccurredAt.Time.Equal(event.occurredAt) {
			return ApplyPolarProjectionResult{}, ErrPolarWebhookConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return ApplyPolarProjectionResult{}, fmt.Errorf("apply Polar projection: commit replay: %w", err)
		}
		return ApplyPolarProjectionResult{}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ApplyPolarProjectionResult{}, fmt.Errorf("apply Polar projection: load receipt: %w", err)
	}
	if err := queries.InsertPolarWebhookReceipt(ctx, dbsql.InsertPolarWebhookReceiptParams{
		ExternalEventID: event.externalEventID, EventType: event.eventType, PayloadSha256: payloadSHA256[:],
		OccurredAt: timestamp(event.occurredAt), ReceivedAt: timestamp(receivedAt),
	}); err != nil {
		return ApplyPolarProjectionResult{}, fmt.Errorf("apply Polar projection: register receipt: %w", err)
	}
	if err := registerPolarCustomer(ctx, queries, event); err != nil {
		return ApplyPolarProjectionResult{}, err
	}
	result := ApplyPolarProjectionResult{Applied: true}
	switch typed := projection.(type) {
	case billing.PolarSubscriptionProjection:
		err = queries.UpsertSubscription(ctx, dbsql.UpsertSubscriptionParams{
			UserID: typed.ExternalCustomerID, PolarSubscriptionID: typed.SubscriptionID,
			PolarCustomerID: typed.CustomerID, Status: string(typed.Status),
			CurrentPeriodStart: timestamp(typed.CurrentPeriodStart), CurrentPeriodEnd: timestamp(typed.CurrentPeriodEnd),
			CancelAtPeriodEnd: typed.CancelAtPeriodEnd, CanceledAt: optionalTimestamp(typed.CanceledAt),
			ExternalEventID: typed.ExternalEventID, ObservedAt: timestamp(typed.OccurredAt),
		})
	case billing.PolarRecurringCreditGrantProjection:
		var transaction billing.CreditTransaction
		transaction, err = applyRecurringCreditGrant(ctx, queries, typed, receivedAt)
		if err == nil {
			result.CreditTransaction = &transaction
		}
	default:
		err = errors.New("apply Polar projection: unsupported projection")
	}
	if err != nil {
		return ApplyPolarProjectionResult{}, fmt.Errorf("apply Polar projection: update projection: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyPolarProjectionResult{}, fmt.Errorf("apply Polar projection: commit: %w", err)
	}
	return result, nil
}

func (store *Store) Subscription(ctx context.Context, userID string) (SubscriptionProjection, bool, error) {
	row, err := store.queries.GetSubscription(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubscriptionProjection{}, false, nil
	}
	if err != nil {
		return SubscriptionProjection{}, false, fmt.Errorf("load Subscription: %w", err)
	}
	if !row.CurrentPeriodStart.Valid || !row.CurrentPeriodEnd.Valid || !row.ObservedAt.Valid {
		return SubscriptionProjection{}, false, errors.New("load Subscription: database returned invalid timestamps")
	}
	projection := SubscriptionProjection{
		UserID: row.UserID, PolarSubscriptionID: row.PolarSubscriptionID, PolarCustomerID: row.PolarCustomerID,
		Status: row.Status, CurrentPeriodStart: row.CurrentPeriodStart.Time, CurrentPeriodEnd: row.CurrentPeriodEnd.Time,
		CancelAtPeriodEnd: row.CancelAtPeriodEnd, ExternalEventID: row.ExternalEventID, ObservedAt: row.ObservedAt.Time,
	}
	if row.CanceledAt.Valid {
		canceledAt := row.CanceledAt.Time
		projection.CanceledAt = &canceledAt
	}
	return projection, true, nil
}

func (store *Store) PolarCustomer(ctx context.Context, userID string) (PolarCustomerProjection, bool, error) {
	row, err := store.queries.GetPolarCustomer(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolarCustomerProjection{}, false, nil
	}
	if err != nil {
		return PolarCustomerProjection{}, false, fmt.Errorf("load Polar Customer: %w", err)
	}
	if !row.ObservedAt.Valid {
		return PolarCustomerProjection{}, false, errors.New("load Polar Customer: database returned invalid observation time")
	}
	return PolarCustomerProjection{
		UserID: row.UserID, PolarCustomerID: row.PolarCustomerID,
		ExternalEventID: row.ExternalEventID, ObservedAt: row.ObservedAt.Time,
	}, true, nil
}

type polarProjectionMetadata struct {
	externalEventID string
	eventType       string
	occurredAt      time.Time
	userID          string
	polarCustomerID string
}

func polarProjectionEvent(projection billing.PolarProjection) (polarProjectionMetadata, error) {
	switch typed := projection.(type) {
	case billing.PolarSubscriptionProjection:
		return polarProjectionMetadata{
			externalEventID: typed.ExternalEventID, eventType: string(typed.EventType), occurredAt: typed.OccurredAt,
			userID: typed.ExternalCustomerID, polarCustomerID: typed.CustomerID,
		}, nil
	case billing.PolarRecurringCreditGrantProjection:
		return polarProjectionMetadata{
			externalEventID: typed.ExternalEventID, eventType: "benefit_grant.cycled", occurredAt: typed.OccurredAt,
			userID: typed.ExternalCustomerID, polarCustomerID: typed.CustomerID,
		}, nil
	default:
		return polarProjectionMetadata{}, errors.New("apply Polar projection: unsupported projection")
	}
}

func registerPolarCustomer(ctx context.Context, queries *dbsql.Queries, event polarProjectionMetadata) error {
	if event.externalEventID == "" || event.eventType == "" || event.occurredAt.IsZero() || event.userID == "" || event.polarCustomerID == "" {
		return errors.New("apply Polar projection: projection identity is incomplete")
	}
	if _, err := queries.InsertPolarCustomer(ctx, dbsql.InsertPolarCustomerParams{
		UserID: event.userID, PolarCustomerID: event.polarCustomerID,
		ExternalEventID: event.externalEventID, ObservedAt: timestamp(event.occurredAt),
	}); err != nil {
		return fmt.Errorf("apply Polar projection: register Polar Customer: %w", err)
	}
	stored, err := queries.GetPolarCustomer(ctx, event.userID)
	if err != nil {
		return fmt.Errorf("apply Polar projection: load Polar Customer: %w", err)
	}
	if stored.PolarCustomerID != event.polarCustomerID {
		return ErrPolarWebhookConflict
	}
	return nil
}

func applyRecurringCreditGrant(
	ctx context.Context,
	queries *dbsql.Queries,
	projection billing.PolarRecurringCreditGrantProjection,
	receivedAt time.Time,
) (billing.CreditTransaction, error) {
	transaction, err := billing.NewGrant(
		"credit-transaction:polar:"+projection.ExternalEventID,
		projection.ExternalCustomerID,
		projection.Credits,
		"polar-webhook:"+projection.ExternalEventID,
		projection.CreditedAt,
		receivedAt,
	)
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("derive recurring Credit Transaction: %w", err)
	}
	if err := queries.InsertCreditTransaction(ctx, dbsql.InsertCreditTransactionParams{
		ID: transaction.ID(), UserID: transaction.UserID(), Kind: string(transaction.Kind()),
		Credits: transaction.Credits(), IdempotencyKey: transaction.IdempotencyKey(),
		OccurredAt: timestamp(transaction.OccurredAt()), CreatedAt: timestamp(transaction.CreatedAt()),
	}); err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("append recurring Credit Transaction: %w", err)
	}
	if _, err := queries.ApplyCreditBalanceTransaction(ctx, dbsql.ApplyCreditBalanceTransactionParams{
		UserID: transaction.UserID(), Credits: transaction.Credits(), UpdatedAt: timestamp(transaction.CreatedAt()),
	}); err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("update Credit Balance: %w", err)
	}
	if err := queries.InsertPolarRecurringCreditGrant(ctx, dbsql.InsertPolarRecurringCreditGrantParams{
		ExternalEventID: projection.ExternalEventID, GrantID: projection.GrantID,
		UserID: projection.ExternalCustomerID, PolarSubscriptionID: projection.SubscriptionID,
		PolarCustomerID: projection.CustomerID, MeterID: projection.MeterID,
		Credits: projection.Credits, CreditedAt: timestamp(projection.CreditedAt),
		CreditTransactionID: transaction.ID(),
	}); err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("record recurring Credit Grant: %w", err)
	}
	return transaction, nil
}
