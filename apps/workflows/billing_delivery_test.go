//go:build !race

// Restate SDK v1.0.0's test HTTP/2 server races in its request-body drain path.
// Keep the real-server workflow tracer in normal tests; race-test the adapters separately.
package workflows_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/restatedev/sdk-go/ingress"
)

func TestBillingDeliveryWorkflowDeliversImmutableOutboxAndCompletesOnce(t *testing.T) {
	event := newCreditsUsedEvent(t)
	store := &polarDeliveryStoreFake{event: event}
	client := &polarEventClientFake{}
	deliveredAt := time.Date(2026, time.July, 13, 12, 1, 0, 0, time.UTC)
	environment := testfixtures.StartRestate(t, workflows.BillingDeliveryDefinition(client, store, func() time.Time { return deliveredAt }))
	workflowClient := workflows.NewClient(environment.Ingress())
	input := workflows.BillingDeliveryInput{ExternalID: event.ExternalID()}

	if err := workflowClient.SendBillingDelivery(t.Context(), input); err != nil {
		t.Fatalf("submit billing delivery workflow: %v", err)
	}
	handle := ingress.WorkflowHandle[workflows.BillingDeliveryOutput](
		environment.Ingress(), workflows.BillingDeliveryService, input.ExternalID,
	)
	output, err := handle.Attach(t.Context())
	if err != nil {
		t.Fatalf("await billing delivery workflow: %v", err)
	}
	if output.ExternalID != input.ExternalID || !output.Delivered {
		t.Fatalf("billing delivery output = %#v", output)
	}
	if calls, externalID := client.snapshot(); calls != 1 || externalID != input.ExternalID {
		t.Fatalf("Polar deliveries = calls:%d externalID:%q", calls, externalID)
	}
	if calls, at := store.completion(); calls != 1 || !at.Equal(deliveredAt) {
		t.Fatalf("PolarDelivery completions = calls:%d at:%s", calls, at)
	}

	if _, err := handle.Attach(t.Context()); err != nil {
		t.Fatalf("replay billing delivery workflow: %v", err)
	}
	if calls, _ := client.snapshot(); calls != 1 {
		t.Fatalf("Polar deliveries after replay = %d", calls)
	}
	if calls, _ := store.completion(); calls != 1 {
		t.Fatalf("PolarDelivery completions after replay = %d", calls)
	}
}

type polarDeliveryStoreFake struct {
	mu              sync.Mutex
	event           billing.CreditsUsedEvent
	delivered       bool
	completionCalls int
	deliveredAt     time.Time
}

func (fake *polarDeliveryStoreFake) PolarDeliveryEvent(_ context.Context, _ string) (billing.CreditsUsedEvent, bool, bool, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.event, fake.delivered, true, nil
}

func (fake *polarDeliveryStoreFake) RecordPolarDeliverySuccess(_ context.Context, _ string, at time.Time) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.completionCalls++
	fake.delivered = true
	fake.deliveredAt = at
	return nil
}

func (fake *polarDeliveryStoreFake) completion() (int, time.Time) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.completionCalls, fake.deliveredAt
}

type polarEventClientFake struct {
	mu         sync.Mutex
	calls      int
	externalID string
}

func (fake *polarEventClientFake) Deliver(_ context.Context, event billing.CreditsUsedEvent) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.externalID = event.ExternalID()
	return nil
}

func (fake *polarEventClientFake) snapshot() (int, string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls, fake.externalID
}

func newCreditsUsedEvent(t *testing.T) billing.CreditsUsedEvent {
	t.Helper()
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	rate, err := billing.NewCreditRate(
		"compute-us-east-1-standard-v1", billing.ResourceCompute, "us-east-1", "standard", "second", "2", createdAt.Add(-time.Hour),
	)
	if err != nil {
		t.Fatalf("create Credit Rate: %v", err)
	}
	transaction, err := billing.NewDebit(
		"credit-transaction:cui_01", "user-1", "compute:cui_01",
		billing.DebitMeasurement{EnvironmentID: "environment-1", ResourceID: "run_01", RawQuantity: "60"},
		rate, createdAt, createdAt,
	)
	if err != nil {
		t.Fatalf("create Credit Transaction: %v", err)
	}
	event, err := billing.NewCreditsUsedEvent(transaction)
	if err != nil {
		t.Fatalf("create credits_used event: %v", err)
	}
	return event
}
