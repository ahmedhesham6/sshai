package billing_test

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
)

func TestCreditRateConvertsDecimalQuantityExactly(t *testing.T) {
	t.Parallel()

	rate, err := billing.NewCreditRate(
		"compute-us-east-1-small-v1",
		billing.ResourceCompute,
		"us-east-1",
		"small",
		"second",
		"0.000000001",
		time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("create credit rate: %v", err)
	}

	credits, err := rate.Convert("1000000000")
	if err != nil {
		t.Fatalf("convert quantity: %v", err)
	}
	if credits != 1 {
		t.Fatalf("credits = %d, want 1", credits)
	}
}

func TestCreditRateRejectsIncompleteResourceIdentity(t *testing.T) {
	t.Parallel()

	_, err := billing.NewCreditRate(
		"",
		billing.ResourceCompute,
		"us-east-1",
		"small",
		"second",
		"1",
		time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
	)
	if err == nil {
		t.Fatal("create credit rate succeeded with an empty version")
	}
}

func TestZeroCreditRateReturnsAnError(t *testing.T) {
	t.Parallel()

	var rate billing.CreditRate
	if _, err := rate.Convert("1"); err == nil {
		t.Fatal("zero credit rate conversion succeeded")
	}
}

func TestCreditRateRetainsImmutableVersionedConfiguration(t *testing.T) {
	t.Parallel()

	effectiveAt := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	rate, err := billing.NewCreditRate(
		"compute-v1",
		billing.ResourceCompute,
		"us-east-1",
		"small",
		"second",
		"0.1250",
		effectiveAt,
	)
	if err != nil {
		t.Fatalf("create rate: %v", err)
	}
	if rate.Version() != "compute-v1" || rate.ResourceType() != billing.ResourceCompute ||
		rate.Region() != "us-east-1" || rate.Preset() != "small" || rate.RawUnit() != "second" ||
		rate.CreditsPerUnit() != "0.1250" || rate.EffectiveAt() != effectiveAt {
		t.Fatal("credit rate did not retain its versioned configuration")
	}
}
