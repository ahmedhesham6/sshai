package billing_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
)

func TestPolarEventClientAuthenticatesAndTreatsAcceptedOrReplayResponsesAsDelivered(t *testing.T) {
	t.Parallel()

	event := creditsUsedFixture(t)
	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var authorization, contentType, body string
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				authorization = request.Header.Get("Authorization")
				contentType = request.Header.Get("Content-Type")
				payload, _ := io.ReadAll(request.Body)
				body = string(payload)
				writer.WriteHeader(status)
			}))
			defer server.Close()
			client, err := billing.NewPolarEventClient(server.URL, "polar-secret-token", server.Client())
			if err != nil {
				t.Fatalf("create client: %v", err)
			}

			if err := client.Deliver(t.Context(), event); err != nil {
				t.Fatalf("deliver: %v", err)
			}
			if authorization != "Bearer polar-secret-token" || contentType != "application/json" ||
				!strings.Contains(body, `"events":[{"name":"credits_used"`) || !strings.Contains(body, `"external_id":"compute:runtime-1:window-9"`) {
				t.Fatalf("request auth=%q content-type=%q body=%s", authorization, contentType, body)
			}
		})
	}
}

func TestPolarEventClientClassifiesRetryableAndTerminalResponsesWithoutLeakingSecrets(t *testing.T) {
	t.Parallel()

	event := creditsUsedFixture(t)
	for _, test := range []struct {
		status    int
		retryable bool
	}{
		{status: http.StatusBadRequest},
		{status: http.StatusUnauthorized},
		{status: http.StatusTooManyRequests, retryable: true},
		{status: http.StatusInternalServerError, retryable: true},
		{status: http.StatusServiceUnavailable, retryable: true},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte("private-customer-payload"))
			}))
			defer server.Close()
			client, err := billing.NewPolarEventClient(server.URL, "polar-secret-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}

			deliveryErr := client.Deliver(t.Context(), event)
			if deliveryErr == nil {
				t.Fatal("delivery error = nil")
			}
			var retryable *billing.PolarRetryableError
			var terminal *billing.PolarTerminalError
			if errors.As(deliveryErr, &retryable) != test.retryable || errors.As(deliveryErr, &terminal) == test.retryable {
				t.Fatalf("delivery error type = %T, retryable=%v", deliveryErr, test.retryable)
			}
			if strings.Contains(deliveryErr.Error(), "polar-secret-token") || strings.Contains(deliveryErr.Error(), "private-customer-payload") {
				t.Fatalf("delivery error leaked protected material: %v", deliveryErr)
			}
		})
	}
}

func TestPolarEventClientRejectsInsecureEndpoints(t *testing.T) {
	t.Parallel()

	if _, err := billing.NewPolarEventClient("http://api.polar.example/v1/events/ingest", "polar-secret-token", http.DefaultClient); err == nil {
		t.Fatal("client accepted an endpoint that would expose the bearer token over plain HTTP")
	}
}

func TestPolarEventClientHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	client, err := billing.NewPolarEventClient("https://api.polar.invalid/v1/events/ingest", "polar-secret-token", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	event := creditsUsedFixture(t)
	go func() { errCh <- client.Deliver(ctx, event) }()
	<-started
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("delivery error = %v, want context canceled", err)
	}
}

func TestPolarEventClientBoundsResponseConsumption(t *testing.T) {
	t.Parallel()

	body := &countingBody{remaining: 1 << 20}
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: body, Header: make(http.Header), Request: request}, nil
	})}
	client, err := billing.NewPolarEventClient("https://api.polar.invalid/v1/events/ingest", "polar-secret-token", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	var retryable *billing.PolarRetryableError
	if err := client.Deliver(t.Context(), creditsUsedFixture(t)); !errors.As(err, &retryable) {
		t.Fatalf("delivery error = %v", err)
	}
	if body.read > 70<<10 {
		t.Fatalf("response bytes consumed = %d, expected a bounded read", body.read)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type countingBody struct {
	remaining int
	read      int
}

func (body *countingBody) Read(buffer []byte) (int, error) {
	if body.remaining == 0 {
		return 0, io.EOF
	}
	count := min(len(buffer), body.remaining)
	for index := range count {
		buffer[index] = 'x'
	}
	body.remaining -= count
	body.read += count
	return count, nil
}

func (*countingBody) Close() error { return nil }

func creditsUsedFixture(t *testing.T) billing.CreditsUsedEvent {
	t.Helper()
	now := time.Date(2026, time.July, 13, 8, 0, 0, 0, time.UTC)
	rate, err := billing.NewCreditRate("compute-v7", billing.ResourceCompute, "eu-west-1", "small", "second", "2", now)
	if err != nil {
		t.Fatal(err)
	}
	debit, err := billing.NewDebit("tx-1", "user-1", "compute:runtime-1:window-9", billing.DebitMeasurement{
		EnvironmentID: "environment-1", ResourceID: "runtime-1", RawQuantity: "15",
	}, rate, now, now)
	if err != nil {
		t.Fatal(err)
	}
	event, err := billing.NewCreditsUsedEvent(debit)
	if err != nil {
		t.Fatal(err)
	}
	return event
}
