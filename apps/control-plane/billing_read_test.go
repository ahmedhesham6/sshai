package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
)

func TestGetBillingSummaryHTTPReportsCreditBalanceWithoutSubscription(t *testing.T) {
	reads := &billingReaderFake{balance: db.CreditBalanceProjection{UserID: "user-1", Credits: 500}}
	handler := billingHandler(reads, []string{"request-billing"})
	response := serveBillingRequest(handler)

	if response.Code != http.StatusOK || response.Header().Get("X-Request-ID") != "request-billing" {
		t.Fatalf("status = %d, request:%q, body:%s", response.Code, response.Header().Get("X-Request-ID"), response.Body.String())
	}
	var summary contracts.BillingSummary
	if err := json.NewDecoder(response.Body).Decode(&summary); err != nil {
		t.Fatalf("decode BillingSummary: %v", err)
	}
	if summary.CreditBalance != 500 || summary.SubscriptionStatus != "none" || summary.CurrentPeriodEnd != nil {
		t.Fatalf("BillingSummary = %#v", summary)
	}
}

func TestGetBillingSummaryHTTPReportsActiveSubscription(t *testing.T) {
	periodEnd := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	reads := &billingReaderFake{
		balance:             db.CreditBalanceProjection{UserID: "user-1", Credits: 1000},
		subscription:        db.SubscriptionProjection{UserID: "user-1", Status: "active", CurrentPeriodEnd: periodEnd},
		subscriptionPresent: true,
	}
	handler := billingHandler(reads, []string{"request-billing"})
	response := serveBillingRequest(handler)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
	}
	var summary contracts.BillingSummary
	if err := json.NewDecoder(response.Body).Decode(&summary); err != nil {
		t.Fatalf("decode BillingSummary: %v", err)
	}
	if summary.CreditBalance != 1000 || summary.SubscriptionStatus != "active" || summary.CurrentPeriodEnd == nil || !summary.CurrentPeriodEnd.Equal(periodEnd) {
		t.Fatalf("BillingSummary = %#v", summary)
	}
}

func TestGetBillingSummaryHTTPMapsUnavailableSafely(t *testing.T) {
	reads := &billingReaderFake{err: errors.New("postgres password=secret")}
	handler := billingHandler(reads, []string{"request-error"})
	response := serveBillingRequest(handler)

	if response.Code != http.StatusServiceUnavailable || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"COMMAND_UNAVAILABLE"`)) || bytes.Contains(response.Body.Bytes(), []byte("password=secret")) {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body.String())
	}
}

func billingHandler(reads controlplane.BillingReader, requestIDs []string) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		BillingReads: reads, Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: repeatValue("user-1", 5)},
		RequestIDs: &idsFake{values: requestIDs}, DefaultRegion: "us-east-1", Now: time.Now,
	})
}

func serveBillingRequest(handler http.Handler) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/billing", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type billingReaderFake struct {
	balance             db.CreditBalanceProjection
	subscription        db.SubscriptionProjection
	subscriptionPresent bool
	err                 error
}

func (fake *billingReaderFake) CreditBalance(_ context.Context, _ string) (db.CreditBalanceProjection, error) {
	if fake.err != nil {
		return db.CreditBalanceProjection{}, fake.err
	}
	return fake.balance, nil
}

func (fake *billingReaderFake) Subscription(_ context.Context, _ string) (db.SubscriptionProjection, bool, error) {
	if fake.err != nil {
		return db.SubscriptionProjection{}, false, fake.err
	}
	return fake.subscription, fake.subscriptionPresent, nil
}
