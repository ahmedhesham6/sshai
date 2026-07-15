package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
)

func (store *Store) RegisterCreditRate(ctx context.Context, rate billing.CreditRate) (billing.CreditRate, error) {
	inserted, err := store.queries.InsertCreditRate(ctx, dbsql.InsertCreditRateParams{
		Version: rate.Version(), ResourceType: string(rate.ResourceType()), Region: rate.Region(),
		RuntimePreset: optionalString(rate.Preset()), RawUnit: rate.RawUnit(),
		CreditsPerUnit: rate.CreditsPerUnit(), EffectiveAt: timestamp(rate.EffectiveAt()),
	})
	if err != nil {
		return billing.CreditRate{}, fmt.Errorf("register Credit Rate: insert: %w", err)
	}
	if inserted == 1 {
		return rate, nil
	}
	row, err := store.queries.GetCreditRate(ctx, rate.Version())
	if err != nil {
		return billing.CreditRate{}, fmt.Errorf("register Credit Rate: load replay: %w", err)
	}
	stored, err := restoreCreditRate(row)
	if err != nil {
		return billing.CreditRate{}, err
	}
	if !sameCreditRate(stored, rate) {
		return billing.CreditRate{}, ErrIdempotencyConflict
	}
	return stored, nil
}

func restoreCreditRate(row dbsql.CreditRate) (billing.CreditRate, error) {
	if !row.EffectiveAt.Valid {
		return billing.CreditRate{}, errors.New("restore Credit Rate: database returned invalid effective time")
	}
	rate, err := billing.NewCreditRate(
		row.Version, billing.ResourceType(row.ResourceType), row.Region,
		optionalStringValue(row.RuntimePreset), row.RawUnit, row.CreditsPerUnit, row.EffectiveAt.Time,
	)
	if err != nil {
		return billing.CreditRate{}, fmt.Errorf("restore Credit Rate: %w", err)
	}
	return rate, nil
}

func sameCreditRate(left, right billing.CreditRate) bool {
	return left.Version() == right.Version() &&
		left.ResourceType() == right.ResourceType() &&
		left.Region() == right.Region() &&
		left.Preset() == right.Preset() &&
		left.RawUnit() == right.RawUnit() &&
		left.CreditsPerUnit() == right.CreditsPerUnit() &&
		left.EffectiveAt().Equal(right.EffectiveAt())
}
