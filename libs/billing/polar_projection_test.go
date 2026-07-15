package billing_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
)

func TestVerifiedPolarWebhookProjectsOnlySubscriptionLifecycleInputs(t *testing.T) {
	t.Parallel()

	for _, eventType := range []billing.PolarSubscriptionEventType{
		billing.PolarSubscriptionCreated,
		billing.PolarSubscriptionUpdated,
		billing.PolarSubscriptionCanceled,
	} {
		t.Run(string(eventType), func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(map[string]any{
				"type": eventType, "timestamp": "2026-07-13T10:00:00Z",
				"data": map[string]any{
					"id": "subscription-1", "status": "active", "current_period_start": "2026-07-01T00:00:00Z",
					"current_period_end": "2026-08-01T00:00:00Z", "cancel_at_period_end": true,
					"canceled_at": "2026-07-13T09:00:00Z", "customer_id": "customer-1",
					"customer": map[string]any{"id": "customer-1", "external_id": "user-1"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			projection, err := verifiedWebhookFixture(t, "polar-event-1", body).Projection()
			if err != nil {
				t.Fatalf("project: %v", err)
			}
			subscription, ok := projection.(billing.PolarSubscriptionProjection)
			if !ok {
				t.Fatalf("projection type = %T", projection)
			}
			if subscription.ExternalEventID != "polar-event-1" || subscription.EventType != eventType ||
				subscription.SubscriptionID != "subscription-1" || subscription.CustomerID != "customer-1" ||
				subscription.ExternalCustomerID != "user-1" || subscription.Status != "active" ||
				!subscription.CancelAtPeriodEnd || subscription.CanceledAt == nil {
				t.Fatalf("subscription projection = %+v", subscription)
			}
		})
	}
}

func TestVerifiedPolarWebhookProjectsRecurringMeterCreditGrant(t *testing.T) {
	t.Parallel()

	body := []byte(`{
  "type":"benefit_grant.cycled",
  "timestamp":"2026-07-13T10:00:00Z",
  "data":{
    "id":"grant-1",
    "is_granted":true,
    "subscription_id":"subscription-1",
    "customer_id":"customer-1",
    "customer":{"id":"customer-1","external_id":"user-1"},
    "benefit":{"type":"meter_credit"},
    "properties":{"last_credited_meter_id":"meter-1","last_credited_units":1000,"last_credited_at":"2026-07-13T09:59:59Z"}
  }
}`)
	projection, err := verifiedWebhookFixture(t, "polar-event-grant-1", body).Projection()
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	grant, ok := projection.(billing.PolarRecurringCreditGrantProjection)
	if !ok {
		t.Fatalf("projection type = %T", projection)
	}
	if grant.ExternalEventID != "polar-event-grant-1" || grant.GrantID != "grant-1" || grant.SubscriptionID != "subscription-1" ||
		grant.CustomerID != "customer-1" || grant.ExternalCustomerID != "user-1" || grant.MeterID != "meter-1" || grant.Credits != 1000 ||
		grant.CreditedAt != time.Date(2026, time.July, 13, 9, 59, 59, 0, time.UTC) {
		t.Fatalf("grant projection = %+v", grant)
	}
}

func TestVerifiedPolarWebhookRejectsUnsupportedOrUnmappedProjectionInputs(t *testing.T) {
	t.Parallel()

	unsupported := []byte(`{"type":"order.paid","timestamp":"2026-07-13T10:00:00Z","data":{"id":"order-1"}}`)
	if _, err := verifiedWebhookFixture(t, "event-1", unsupported).Projection(); !errors.Is(err, billing.ErrUnsupportedPolarWebhookEvent) {
		t.Fatalf("unsupported event error = %v", err)
	}
	unmapped := []byte(`{
  "type":"subscription.created","timestamp":"2026-07-13T10:00:00Z",
  "data":{"id":"subscription-1","status":"active","current_period_start":"2026-07-01T00:00:00Z","current_period_end":"2026-08-01T00:00:00Z","customer_id":"customer-1","customer":{"id":"customer-1"}}
}`)
	if _, err := verifiedWebhookFixture(t, "event-2", unmapped).Projection(); !errors.Is(err, billing.ErrMalformedPolarWebhookPayload) {
		t.Fatalf("unmapped customer error = %v", err)
	}
}

func verifiedWebhookFixture(t *testing.T, id string, body []byte) billing.VerifiedPolarWebhook {
	t.Helper()
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	secret, key := webhookSecret()
	verifier, err := billing.NewPolarWebhookVerifier(secret, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(signedWebhookHeaders(id, now.Unix(), body, key), body, now)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}
