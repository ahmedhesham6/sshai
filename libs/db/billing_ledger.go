package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
)

type CreditBalanceProjection struct {
	UserID    string
	Credits   int64
	Version   int64
	UpdatedAt time.Time
}

func (store *Store) CreditBalance(ctx context.Context, userID string) (CreditBalanceProjection, error) {
	row, err := store.queries.GetCreditBalance(ctx, userID)
	if err != nil {
		return CreditBalanceProjection{}, fmt.Errorf("load Credit Balance: %w", err)
	}
	if !row.UpdatedAt.Valid {
		return CreditBalanceProjection{}, errors.New("load Credit Balance: database returned invalid update time")
	}
	return CreditBalanceProjection{UserID: row.UserID, Credits: row.Credits, Version: row.Version, UpdatedAt: row.UpdatedAt.Time}, nil
}

func loadCreditTransaction(ctx context.Context, queries *dbsql.Queries, id string) (billing.CreditTransaction, error) {
	row, err := queries.GetCreditTransaction(ctx, id)
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("restore Credit Transaction: load: %w", err)
	}
	if !row.OccurredAt.Valid || !row.CreatedAt.Valid {
		return billing.CreditTransaction{}, errors.New("restore Credit Transaction: database returned invalid timestamps")
	}
	var transaction billing.CreditTransaction
	switch billing.TransactionKind(row.Kind) {
	case billing.TransactionDebit:
		if row.RateVersion == nil || row.EnvironmentID == nil || row.ResourceID == nil || row.RawQuantity == nil {
			return billing.CreditTransaction{}, errors.New("restore Credit Transaction: debit usage is incomplete")
		}
		rateRow, err := queries.GetCreditRate(ctx, *row.RateVersion)
		if err != nil {
			return billing.CreditTransaction{}, fmt.Errorf("restore Credit Transaction: load Credit Rate: %w", err)
		}
		rate, err := restoreCreditRate(rateRow)
		if err != nil {
			return billing.CreditTransaction{}, err
		}
		transaction, err = billing.NewDebit(
			row.ID, row.UserID, row.IdempotencyKey,
			billing.DebitMeasurement{EnvironmentID: *row.EnvironmentID, ResourceID: *row.ResourceID, RawQuantity: *row.RawQuantity},
			rate, row.OccurredAt.Time, row.CreatedAt.Time,
		)
		if err != nil {
			return billing.CreditTransaction{}, fmt.Errorf("restore Credit Transaction: %w", err)
		}
		usage, _ := transaction.DebitUsage()
		if transaction.Credits() != row.Credits || !optionalEquals(row.ResourceType, string(usage.ResourceType)) ||
			!optionalEquals(row.Region, usage.Region) || !optionalEquals(row.RawUnit, usage.RawUnit) {
			return billing.CreditTransaction{}, errors.New("restore Credit Transaction: stored debit disagrees with approved Credit Rate")
		}
	case billing.TransactionGrant:
		transaction, err = billing.NewGrant(row.ID, row.UserID, row.Credits, row.IdempotencyKey, row.OccurredAt.Time, row.CreatedAt.Time)
	case billing.TransactionAdjustment:
		transaction, err = billing.NewAdjustment(row.ID, row.UserID, row.Credits, row.IdempotencyKey, row.OccurredAt.Time, row.CreatedAt.Time)
	case billing.TransactionRefund:
		transaction, err = billing.NewRefund(row.ID, row.UserID, row.Credits, row.IdempotencyKey, row.OccurredAt.Time, row.CreatedAt.Time)
	default:
		err = errors.New("unsupported Credit Transaction kind")
	}
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("restore Credit Transaction: %w", err)
	}
	return transaction, nil
}

func optionalEquals(value *string, expected string) bool {
	return value != nil && *value == expected
}
