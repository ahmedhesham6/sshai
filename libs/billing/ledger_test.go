package billing_test

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
)

func TestCreditBalanceProjectsGrantsAndOneIdempotentDebit(t *testing.T) {
	t.Parallel()

	occurredAt := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	balance, err := billing.NewCreditBalance("user-1")
	if err != nil {
		t.Fatalf("create balance: %v", err)
	}
	grant, err := billing.NewGrant("tx-grant", "user-1", 100, "renewal-2026-07", occurredAt, occurredAt)
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	applied, err := balance.Apply(grant)
	if err != nil || !applied {
		t.Fatalf("apply grant: applied=%v err=%v", applied, err)
	}

	rate, err := billing.NewCreditRate("compute-v1", billing.ResourceCompute, "us-east-1", "small", "second", "2", occurredAt)
	if err != nil {
		t.Fatalf("create rate: %v", err)
	}
	debit, err := billing.NewDebit("tx-debit", "user-1", "interval-1", billing.DebitMeasurement{
		EnvironmentID: "environment-1", ResourceID: "runtime-1", RawQuantity: "15",
	}, rate, occurredAt, occurredAt)
	if err != nil {
		t.Fatalf("create debit: %v", err)
	}
	if applied, err = balance.Apply(debit); err != nil || !applied {
		t.Fatalf("apply debit: applied=%v err=%v", applied, err)
	}
	if applied, err = balance.Apply(debit); err != nil || applied {
		t.Fatalf("replay debit: applied=%v err=%v", applied, err)
	}

	if got := balance.Credits(); got != 70 {
		t.Fatalf("balance = %d, want 70", got)
	}
	if got := balance.Version(); got != 2 {
		t.Fatalf("version = %d, want 2", got)
	}
}

func TestCreditBalanceRejectsReusedTransactionIdentity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	balance, err := billing.NewCreditBalance("user-1")
	if err != nil {
		t.Fatalf("create balance: %v", err)
	}
	first, err := billing.NewGrant("tx-1", "user-1", 10, "grant-1", now, now)
	if err != nil {
		t.Fatalf("create first grant: %v", err)
	}
	second, err := billing.NewGrant("tx-1", "user-1", 10, "grant-2", now, now)
	if err != nil {
		t.Fatalf("create second grant: %v", err)
	}
	if _, err = balance.Apply(first); err != nil {
		t.Fatalf("apply first grant: %v", err)
	}
	if _, err = balance.Apply(second); err == nil {
		t.Fatal("reused transaction ID was accepted")
	}
}

func TestZeroCreditBalanceReturnsAnError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	grant, err := billing.NewGrant("tx-1", "user-1", 10, "grant-1", now, now)
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	var balance billing.CreditBalance
	if _, err = balance.Apply(grant); err == nil {
		t.Fatal("zero credit balance accepted a transaction")
	}
}

func TestCreditBalanceProjectsEveryImmutableTransactionKind(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	balance, err := billing.NewCreditBalance("user-1")
	if err != nil {
		t.Fatalf("create balance: %v", err)
	}
	grant, err := billing.NewGrant("grant", "user-1", 100, "renewal", now, now)
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	rateV1, err := billing.NewCreditRate("storage-v1", billing.ResourceStorage, "us-east-1", "", "gib-hour", "3", now)
	if err != nil {
		t.Fatalf("create v1 rate: %v", err)
	}
	debit, err := billing.NewDebit("debit", "user-1", "window-1", billing.DebitMeasurement{
		EnvironmentID: "environment-1", ResourceID: "volume-1", RawQuantity: "10",
	}, rateV1, now, now)
	if err != nil {
		t.Fatalf("create debit: %v", err)
	}
	adjustment, err := billing.NewAdjustment("adjustment", "user-1", -5, "support-1", now, now)
	if err != nil {
		t.Fatalf("create adjustment: %v", err)
	}
	refund, err := billing.NewRefund("refund", "user-1", 10, "refund-1", now, now)
	if err != nil {
		t.Fatalf("create refund: %v", err)
	}
	for _, transaction := range []billing.CreditTransaction{grant, debit, adjustment, refund} {
		if applied, applyErr := balance.Apply(transaction); applyErr != nil || !applied {
			t.Fatalf("apply %s: applied=%v err=%v", transaction.Kind(), applied, applyErr)
		}
	}

	if got := balance.Credits(); got != 75 {
		t.Fatalf("balance = %d, want 75", got)
	}
	if _, err = billing.NewCreditRate("storage-v2", billing.ResourceStorage, "us-east-1", "", "gib-hour", "4", now.Add(time.Hour)); err != nil {
		t.Fatalf("create v2 rate: %v", err)
	}
	usage, ok := debit.DebitUsage()
	if !ok {
		t.Fatal("debit has no usage metadata")
	}
	if usage.RateVersion != "storage-v1" || usage.RawQuantity != "10" || usage.ResourceType != billing.ResourceStorage {
		t.Fatalf("debit usage = %+v, want original storage-v1 conversion", usage)
	}
}

func TestCreditBalanceTreatsEquivalentTimestampRepresentationsAsReplay(t *testing.T) {
	t.Parallel()

	utc := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	sameInstant := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	balance, err := billing.NewCreditBalance("user-1")
	if err != nil {
		t.Fatalf("create balance: %v", err)
	}
	first, err := billing.NewGrant("grant", "user-1", 10, "renewal", utc, utc)
	if err != nil {
		t.Fatalf("create first grant: %v", err)
	}
	replay, err := billing.NewGrant("grant", "user-1", 10, "renewal", sameInstant, sameInstant)
	if err != nil {
		t.Fatalf("create replay grant: %v", err)
	}
	if _, err = balance.Apply(first); err != nil {
		t.Fatalf("apply first grant: %v", err)
	}
	if applied, applyErr := balance.Apply(replay); applyErr != nil || applied {
		t.Fatalf("replay equivalent instant: applied=%v err=%v", applied, applyErr)
	}
}
