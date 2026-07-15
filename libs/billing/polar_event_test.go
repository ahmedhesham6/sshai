package billing_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
)

func TestCreditsUsedEventIsAnImmutableReplayOfAnApprovedDebit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 10, 0, 0, 123, time.FixedZone("UTC+2", 2*60*60))
	rate, err := billing.NewCreditRate("compute-v7", billing.ResourceCompute, "eu-west-1", "small", "second", "2", now)
	if err != nil {
		t.Fatalf("create rate: %v", err)
	}
	debit, err := billing.NewDebit("tx-1", "user-1", "compute:runtime-1:window-9", billing.DebitMeasurement{
		EnvironmentID: "environment-1", ResourceID: "runtime-1", RawQuantity: "15",
	}, rate, now, now)
	if err != nil {
		t.Fatalf("create debit: %v", err)
	}

	first, err := billing.NewCreditsUsedEvent(debit)
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	replay, err := billing.NewCreditsUsedEvent(debit)
	if err != nil {
		t.Fatalf("recreate event: %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	replayJSON, _ := json.Marshal(replay)
	if string(firstJSON) != string(replayJSON) || first.ExternalID() != "compute:runtime-1:window-9" {
		t.Fatalf("replay identity changed: first=%s replay=%s key=%q", firstJSON, replayJSON, first.ExternalID())
	}

	var got struct {
		Name               string         `json:"name"`
		ExternalCustomerID string         `json:"external_customer_id"`
		ExternalID         string         `json:"external_id"`
		Timestamp          time.Time      `json:"timestamp"`
		Metadata           map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(firstJSON, &got); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if got.Name != "credits_used" || got.ExternalCustomerID != "user-1" || got.ExternalID != "compute:runtime-1:window-9" ||
		!got.Timestamp.Equal(now) {
		t.Fatalf("event identity = %+v", got)
	}
	wantMetadata := map[string]any{
		"credits": float64(30), "resource_type": "compute", "environment_id": "environment-1", "region": "eu-west-1",
		"raw_quantity": "15", "raw_unit": "second", "rate_version": "compute-v7",
	}
	if len(got.Metadata) != len(wantMetadata) {
		t.Fatalf("metadata = %#v", got.Metadata)
	}
	for key, want := range wantMetadata {
		if got.Metadata[key] != want {
			t.Fatalf("metadata[%q] = %#v, want %#v", key, got.Metadata[key], want)
		}
	}
}

func TestCreditsUsedEventRejectsNonDebitTransactions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 8, 0, 0, 0, time.UTC)
	grant, err := billing.NewGrant("grant-1", "user-1", 100, "renewal-1", now, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := billing.NewCreditsUsedEvent(grant); err == nil {
		t.Fatal("credits_used event accepted a non-debit Credit Transaction")
	}
}
