package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrRuntimeNotCurrent       = errors.New("Runtime is not current for the owned Environment")
	ErrComputeUsageAlreadyOpen = errors.New("Compute Usage Interval is already open")
)

type OpenComputeUsageIntervalInput struct {
	ID            string
	UserID        string
	EnvironmentID string
	RuntimeID     string
	StartedAt     time.Time
}

type ComputeUsageInterval struct {
	ID                  string
	UserID              string
	EnvironmentID       string
	RuntimeID           string
	Region              string
	RuntimePreset       string
	StartedAt           time.Time
	EndedAt             *time.Time
	ClosureSource       string
	CreditTransactionID string
}

type ComputeUsageClosureSource string

const (
	ComputeUsageClosedByRuntimeStop            ComputeUsageClosureSource = "runtime_stop"
	ComputeUsageClosedByProviderReconciliation ComputeUsageClosureSource = "provider_reconciliation"
)

type CloseComputeUsageIntervalInput struct {
	IntervalID string
	StoppedAt  time.Time
	Source     ComputeUsageClosureSource
}

func (store *Store) OpenComputeUsageInterval(ctx context.Context, input OpenComputeUsageIntervalInput) (ComputeUsageInterval, error) {
	if input.ID == "" || input.UserID == "" || input.EnvironmentID == "" || input.RuntimeID == "" || input.StartedAt.IsZero() {
		return ComputeUsageInterval{}, errors.New("open Compute Usage Interval: identity and start time are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ComputeUsageInterval{}, fmt.Errorf("open Compute Usage Interval: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	environment, err := queries.GetOwnedEnvironmentRuntimeForUpdate(ctx, dbsql.GetOwnedEnvironmentRuntimeForUpdateParams{
		ID: input.EnvironmentID, OwnerUserID: input.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (environment.CurrentRuntimeID == nil || *environment.CurrentRuntimeID != input.RuntimeID) {
		return ComputeUsageInterval{}, ErrRuntimeNotCurrent
	}
	if err != nil {
		return ComputeUsageInterval{}, fmt.Errorf("open Compute Usage Interval: lock Environment: %w", err)
	}
	inserted, err := queries.InsertComputeUsageInterval(ctx, dbsql.InsertComputeUsageIntervalParams{
		ID: input.ID, UserID: input.UserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID,
		Region: environment.Region, RuntimePreset: environment.RuntimePreset, StartedAt: timestamp(input.StartedAt),
	})
	if err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && (pgError.ConstraintName == "compute_usage_intervals_open_environment_key" ||
			pgError.ConstraintName == "compute_usage_intervals_open_runtime_key") {
			return ComputeUsageInterval{}, ErrComputeUsageAlreadyOpen
		}
		return ComputeUsageInterval{}, fmt.Errorf("open Compute Usage Interval: insert: %w", err)
	}
	row, err := queries.GetComputeUsageInterval(ctx, input.ID)
	if err != nil {
		return ComputeUsageInterval{}, fmt.Errorf("open Compute Usage Interval: load: %w", err)
	}
	interval, err := restoreComputeUsageInterval(row)
	if err != nil {
		return ComputeUsageInterval{}, err
	}
	if inserted == 0 && (interval.UserID != input.UserID || interval.EnvironmentID != input.EnvironmentID ||
		interval.RuntimeID != input.RuntimeID || !interval.StartedAt.Equal(input.StartedAt)) {
		return ComputeUsageInterval{}, ErrIdempotencyConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return ComputeUsageInterval{}, fmt.Errorf("open Compute Usage Interval: commit: %w", err)
	}
	return interval, nil
}

func restoreComputeUsageInterval(row dbsql.ComputeUsageInterval) (ComputeUsageInterval, error) {
	if !row.StartedAt.Valid {
		return ComputeUsageInterval{}, errors.New("restore Compute Usage Interval: database returned invalid start time")
	}
	interval := ComputeUsageInterval{
		ID: row.ID, UserID: row.UserID, EnvironmentID: row.EnvironmentID, RuntimeID: row.RuntimeID,
		Region: row.Region, RuntimePreset: row.RuntimePreset, StartedAt: row.StartedAt.Time,
		ClosureSource: optionalStringValue(row.ClosureSource), CreditTransactionID: optionalStringValue(row.CreditTransactionID),
	}
	if row.EndedAt.Valid {
		endedAt := row.EndedAt.Time
		interval.EndedAt = &endedAt
	}
	return interval, nil
}

func (store *Store) CloseComputeUsageInterval(ctx context.Context, input CloseComputeUsageIntervalInput) (billing.CreditTransaction, error) {
	if input.IntervalID == "" || input.StoppedAt.IsZero() ||
		input.Source != ComputeUsageClosedByRuntimeStop && input.Source != ComputeUsageClosedByProviderReconciliation {
		return billing.CreditTransaction{}, errors.New("close Compute Usage Interval: identity, stop time, and closure source are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	row, err := queries.GetComputeUsageIntervalForUpdate(ctx, input.IntervalID)
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: lock interval: %w", err)
	}
	interval, err := restoreComputeUsageInterval(row)
	if err != nil {
		return billing.CreditTransaction{}, err
	}
	if interval.CreditTransactionID != "" {
		transaction, err := loadCreditTransaction(ctx, queries, interval.CreditTransactionID)
		if err != nil {
			return billing.CreditTransaction{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: commit replay: %w", err)
		}
		return transaction, nil
	}
	if !input.StoppedAt.After(interval.StartedAt) {
		return billing.CreditTransaction{}, errors.New("close Compute Usage Interval: stop time must follow start time")
	}
	rateRow, err := queries.GetApplicableComputeCreditRate(ctx, dbsql.GetApplicableComputeCreditRateParams{
		Region: interval.Region, RuntimePreset: optionalString(interval.RuntimePreset), EffectiveAt: timestamp(interval.StartedAt),
	})
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: load approved Credit Rate: %w", err)
	}
	rate, err := restoreCreditRate(rateRow)
	if err != nil {
		return billing.CreditTransaction{}, err
	}
	rawQuantity := decimalSeconds(input.StoppedAt.Sub(interval.StartedAt))
	transaction, err := billing.NewDebit(
		"credit-transaction:"+interval.ID, interval.UserID, "compute:"+interval.ID,
		billing.DebitMeasurement{EnvironmentID: interval.EnvironmentID, ResourceID: interval.RuntimeID, RawQuantity: rawQuantity},
		rate, input.StoppedAt, input.StoppedAt,
	)
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: derive debit: %w", err)
	}
	usage, _ := transaction.DebitUsage()
	if err := queries.InsertCreditTransaction(ctx, dbsql.InsertCreditTransactionParams{
		ID: transaction.ID(), UserID: transaction.UserID(), Kind: string(transaction.Kind()), Credits: transaction.Credits(),
		ResourceType: optionalString(string(usage.ResourceType)), EnvironmentID: optionalString(usage.EnvironmentID),
		ResourceID: optionalString(usage.ResourceID), Region: optionalString(usage.Region), RawQuantity: optionalString(usage.RawQuantity),
		RawUnit: optionalString(usage.RawUnit), RateVersion: optionalString(usage.RateVersion), IdempotencyKey: transaction.IdempotencyKey(),
		OccurredAt: timestamp(transaction.OccurredAt()), CreatedAt: timestamp(transaction.CreatedAt()),
	}); err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: append Credit Transaction: %w", err)
	}
	if _, err := queries.ApplyCreditBalanceTransaction(ctx, dbsql.ApplyCreditBalanceTransactionParams{
		UserID: transaction.UserID(), Credits: transaction.Credits(), UpdatedAt: timestamp(transaction.CreatedAt()),
	}); err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: update Credit Balance: %w", err)
	}
	event, err := billing.NewCreditsUsedEvent(transaction)
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: build Polar event: %w", err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: encode Polar event: %w", err)
	}
	if err := queries.InsertPolarDelivery(ctx, dbsql.InsertPolarDeliveryParams{
		ExternalID: event.ExternalID(), CreditTransactionID: transaction.ID(), EventPayload: payload,
		CreatedAt: timestamp(transaction.CreatedAt()),
	}); err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: create PolarDelivery: %w", err)
	}
	updated, err := queries.CloseComputeUsageInterval(ctx, dbsql.CloseComputeUsageIntervalParams{
		ID: interval.ID, EndedAt: timestamp(input.StoppedAt), ClosureSource: optionalString(string(input.Source)),
		CreditTransactionID: optionalString(transaction.ID()),
	})
	if err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: close interval: %w", err)
	}
	if updated != 1 {
		return billing.CreditTransaction{}, errors.New("close Compute Usage Interval: concurrent interval update")
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.CreditTransaction{}, fmt.Errorf("close Compute Usage Interval: commit: %w", err)
	}
	return transaction, nil
}

func decimalSeconds(duration time.Duration) string {
	nanoseconds := duration.Nanoseconds()
	seconds, remainder := nanoseconds/int64(time.Second), nanoseconds%int64(time.Second)
	if remainder == 0 {
		return strconv.FormatInt(seconds, 10)
	}
	fraction := strings.TrimRight(fmt.Sprintf("%09d", remainder), "0")
	return strconv.FormatInt(seconds, 10) + "." + fraction
}
