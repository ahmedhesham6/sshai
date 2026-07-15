package billing

import (
	"errors"
	"math"
	"time"
)

type TransactionKind string

const (
	TransactionGrant      TransactionKind = "grant"
	TransactionDebit      TransactionKind = "debit"
	TransactionAdjustment TransactionKind = "adjustment"
	TransactionRefund     TransactionKind = "refund"
)

var ErrIdempotencyConflict = errors.New("credit transaction idempotency conflict")

var ErrTransactionIDConflict = errors.New("credit transaction ID conflict")

type DebitUsage struct {
	ResourceType  ResourceType
	EnvironmentID string
	ResourceID    string
	Region        string
	RawQuantity   string
	RawUnit       string
	RateVersion   string
}

type DebitMeasurement struct {
	EnvironmentID string
	ResourceID    string
	RawQuantity   string
}

type CreditTransaction struct {
	id             string
	userID         string
	kind           TransactionKind
	credits        int64
	idempotencyKey string
	usage          DebitUsage
	occurredAt     time.Time
	createdAt      time.Time
}

func NewGrant(id, userID string, credits int64, idempotencyKey string, occurredAt, createdAt time.Time) (CreditTransaction, error) {
	if credits <= 0 {
		return CreditTransaction{}, errors.New("grant credits must be positive")
	}
	return newCreditTransaction(id, userID, TransactionGrant, credits, idempotencyKey, DebitUsage{}, occurredAt, createdAt)
}

func NewAdjustment(id, userID string, credits int64, idempotencyKey string, occurredAt, createdAt time.Time) (CreditTransaction, error) {
	if credits == 0 {
		return CreditTransaction{}, errors.New("adjustment credits cannot be zero")
	}
	return newCreditTransaction(id, userID, TransactionAdjustment, credits, idempotencyKey, DebitUsage{}, occurredAt, createdAt)
}

func NewRefund(id, userID string, credits int64, idempotencyKey string, occurredAt, createdAt time.Time) (CreditTransaction, error) {
	if credits <= 0 {
		return CreditTransaction{}, errors.New("refund credits must be positive")
	}
	return newCreditTransaction(id, userID, TransactionRefund, credits, idempotencyKey, DebitUsage{}, occurredAt, createdAt)
}

func NewDebit(
	id, userID, idempotencyKey string,
	measurement DebitMeasurement,
	rate CreditRate,
	occurredAt, createdAt time.Time,
) (CreditTransaction, error) {
	credits, err := rate.Convert(measurement.RawQuantity)
	if err != nil {
		return CreditTransaction{}, err
	}
	if credits <= 0 || measurement.EnvironmentID == "" || measurement.ResourceID == "" {
		return CreditTransaction{}, errors.New("debit requires positive credits and Environment and resource IDs")
	}
	usage := DebitUsage{
		ResourceType:  rate.resourceType,
		EnvironmentID: measurement.EnvironmentID,
		ResourceID:    measurement.ResourceID,
		Region:        rate.region,
		RawQuantity:   measurement.RawQuantity,
		RawUnit:       rate.rawUnit,
		RateVersion:   rate.version,
	}
	return newCreditTransaction(id, userID, TransactionDebit, -credits, idempotencyKey, usage, occurredAt, createdAt)
}

func newCreditTransaction(
	id, userID string,
	kind TransactionKind,
	credits int64,
	idempotencyKey string,
	usage DebitUsage,
	occurredAt, createdAt time.Time,
) (CreditTransaction, error) {
	if id == "" || userID == "" || idempotencyKey == "" || occurredAt.IsZero() || createdAt.IsZero() {
		return CreditTransaction{}, errors.New("transaction identity, idempotency key, and timestamps are required")
	}
	if createdAt.Before(occurredAt) {
		return CreditTransaction{}, errors.New("transaction cannot be created before it occurred")
	}
	return CreditTransaction{
		id:             id,
		userID:         userID,
		kind:           kind,
		credits:        credits,
		idempotencyKey: idempotencyKey,
		usage:          usage,
		occurredAt:     occurredAt.Round(0).UTC(),
		createdAt:      createdAt.Round(0).UTC(),
	}, nil
}

func (t CreditTransaction) ID() string { return t.id }

func (t CreditTransaction) UserID() string { return t.userID }

func (t CreditTransaction) Kind() TransactionKind { return t.kind }

func (t CreditTransaction) Credits() int64 { return t.credits }

func (t CreditTransaction) IdempotencyKey() string { return t.idempotencyKey }

func (t CreditTransaction) DebitUsage() (DebitUsage, bool) {
	return t.usage, t.kind == TransactionDebit
}

func (t CreditTransaction) OccurredAt() time.Time { return t.occurredAt }

func (t CreditTransaction) CreatedAt() time.Time { return t.createdAt }

type CreditBalance struct {
	userID         string
	credits        int64
	version        int64
	updatedAt      time.Time
	transactions   map[string]CreditTransaction
	transactionIDs map[string]string
}

func NewCreditBalance(userID string) (*CreditBalance, error) {
	if userID == "" {
		return nil, errors.New("credit balance user ID is required")
	}
	return &CreditBalance{
		userID:         userID,
		transactions:   make(map[string]CreditTransaction),
		transactionIDs: make(map[string]string),
	}, nil
}

func (b *CreditBalance) Apply(transaction CreditTransaction) (bool, error) {
	if b == nil || b.userID == "" || b.transactions == nil || b.transactionIDs == nil {
		return false, errors.New("credit balance is not initialized")
	}
	if transaction.userID != b.userID {
		return false, errors.New("credit transaction belongs to another user")
	}
	if existing, ok := b.transactions[transaction.idempotencyKey]; ok {
		if existing == transaction {
			return false, nil
		}
		return false, ErrIdempotencyConflict
	}
	if existingKey, ok := b.transactionIDs[transaction.id]; ok && existingKey != transaction.idempotencyKey {
		return false, ErrTransactionIDConflict
	}
	if transaction.credits > 0 && b.credits > math.MaxInt64-transaction.credits ||
		transaction.credits < 0 && b.credits < math.MinInt64-transaction.credits {
		return false, errors.New("credit balance overflow")
	}
	b.credits += transaction.credits
	b.version++
	b.updatedAt = transaction.createdAt
	b.transactions[transaction.idempotencyKey] = transaction
	b.transactionIDs[transaction.id] = transaction.idempotencyKey
	return true, nil
}

func (b *CreditBalance) Credits() int64 { return b.credits }

func (b *CreditBalance) Version() int64 { return b.version }
