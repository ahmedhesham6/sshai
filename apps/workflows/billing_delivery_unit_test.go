package workflows

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
	restate "github.com/restatedev/sdk-go"
)

func TestDeliverPendingPolarEventClassifiesExternalOutcomes(t *testing.T) {
	event := billingDeliveryTestEvent(t)
	for _, test := range []struct {
		name          string
		status        int
		delivered     bool
		wantDelivered bool
		wantTerminal  bool
		wantRetryable bool
	}{
		{name: "accepted", status: http.StatusAccepted, wantDelivered: true},
		{name: "conflict replay", status: http.StatusConflict, wantDelivered: true},
		{name: "already delivered", status: http.StatusBadRequest, delivered: true},
		{name: "terminal rejection", status: http.StatusBadRequest, wantTerminal: true},
		{name: "retryable rejection", status: http.StatusServiceUnavailable, wantRetryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
			}))
			defer server.Close()
			client, err := billing.NewPolarEventClient(server.URL, "polar-token", server.Client())
			if err != nil {
				t.Fatalf("create Polar client: %v", err)
			}
			store := &deliveryActionStore{event: event, delivered: test.delivered, found: true}
			delivered, err := deliverPendingPolarEvent(context.Background(), client, store, event.ExternalID())
			if delivered != test.wantDelivered {
				t.Fatalf("delivered now = %t, want %t", delivered, test.wantDelivered)
			}
			var terminal restate.TerminalError
			if errors.As(err, &terminal) != test.wantTerminal {
				t.Fatalf("terminal error = %v, want terminal:%t", err, test.wantTerminal)
			}
			var retryable *billing.PolarRetryableError
			if errors.As(err, &retryable) != test.wantRetryable {
				t.Fatalf("retryable error = %v, want retryable:%t", err, test.wantRetryable)
			}
			if test.delivered && store.loadCount() != 1 {
				t.Fatalf("already-delivered outbox loads = %d", store.loadCount())
			}
		})
	}
}

func TestDeliverPendingPolarEventFailsClosedWhenOutboxIsMissing(t *testing.T) {
	store := &deliveryActionStore{}
	delivered, err := deliverPendingPolarEvent(context.Background(), &countingDeliverer{}, store, "compute:missing")
	var terminal restate.TerminalError
	if delivered || !errors.As(err, &terminal) {
		t.Fatalf("missing outbox result = delivered:%t error:%v", delivered, err)
	}
}

type deliveryActionStore struct {
	mu        sync.Mutex
	event     billing.CreditsUsedEvent
	delivered bool
	found     bool
	loads     int
}

func (store *deliveryActionStore) PolarDeliveryEvent(_ context.Context, _ string) (billing.CreditsUsedEvent, bool, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.loads++
	return store.event, store.delivered, store.found, nil
}

func (store *deliveryActionStore) RecordPolarDeliverySuccess(context.Context, string, time.Time) error {
	return nil
}

func (store *deliveryActionStore) loadCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.loads
}

type countingDeliverer struct {
	mu    sync.Mutex
	calls int
}

func (deliverer *countingDeliverer) Deliver(context.Context, billing.CreditsUsedEvent) error {
	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	deliverer.calls++
	return nil
}

func billingDeliveryTestEvent(t *testing.T) billing.CreditsUsedEvent {
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
