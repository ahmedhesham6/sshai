package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/jackc/pgx/v5"
)

type PolarDeliveryProjection struct {
	ExternalID  string
	Transaction billing.CreditTransaction
	Event       billing.CreditsUsedEvent
	CreatedAt   time.Time
	DeliveredAt *time.Time
}

func (store *Store) PolarDelivery(ctx context.Context, externalID string) (PolarDeliveryProjection, bool, error) {
	row, err := store.queries.GetPolarDelivery(ctx, externalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolarDeliveryProjection{}, false, nil
	}
	if err != nil {
		return PolarDeliveryProjection{}, false, fmt.Errorf("load PolarDelivery: %w", err)
	}
	delivery, err := restorePolarDelivery(ctx, store.queries, row)
	return delivery, true, err
}

func (store *Store) PolarDeliveryEvent(ctx context.Context, externalID string) (billing.CreditsUsedEvent, bool, bool, error) {
	delivery, found, err := store.PolarDelivery(ctx, externalID)
	if err != nil || !found {
		return billing.CreditsUsedEvent{}, false, found, err
	}
	return delivery.Event, delivery.DeliveredAt != nil, true, nil
}

func (store *Store) RecordPolarDeliverySuccess(ctx context.Context, externalID string, deliveredAt time.Time) error {
	if externalID == "" || deliveredAt.IsZero() {
		return errors.New("record PolarDelivery success: external ID and delivery time are required")
	}
	row, err := store.queries.CompletePolarDelivery(ctx, dbsql.CompletePolarDeliveryParams{
		ExternalID: externalID, DeliveredAt: timestamp(deliveredAt),
	})
	if err != nil {
		return fmt.Errorf("record PolarDelivery success: %w", err)
	}
	_, err = restorePolarDelivery(ctx, store.queries, row)
	if err != nil {
		return err
	}
	return nil
}

func restorePolarDelivery(ctx context.Context, queries *dbsql.Queries, row dbsql.PolarDelivery) (PolarDeliveryProjection, error) {
	transaction, err := loadCreditTransaction(ctx, queries, row.CreditTransactionID)
	if err != nil {
		return PolarDeliveryProjection{}, err
	}
	event, err := billing.NewCreditsUsedEvent(transaction)
	if err != nil {
		return PolarDeliveryProjection{}, fmt.Errorf("load PolarDelivery: restore event: %w", err)
	}
	payload, err := json.Marshal(event)
	if err != nil || !sameJSON(payload, row.EventPayload) {
		return PolarDeliveryProjection{}, errors.New("load PolarDelivery: stored event does not match immutable Credit Transaction")
	}
	if !row.CreatedAt.Valid {
		return PolarDeliveryProjection{}, errors.New("load PolarDelivery: database returned invalid creation time")
	}
	delivery := PolarDeliveryProjection{
		ExternalID: row.ExternalID, Transaction: transaction, Event: event, CreatedAt: row.CreatedAt.Time,
	}
	if row.DeliveredAt.Valid {
		deliveredAt := row.DeliveredAt.Time
		delivery.DeliveredAt = &deliveredAt
	}
	return delivery, nil
}
