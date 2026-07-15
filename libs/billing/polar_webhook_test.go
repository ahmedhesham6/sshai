package billing_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
)

func TestPolarWebhookVerifierAuthenticatesTheExactRawBodyAndRotationSignatures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	secret, key := webhookSecret()
	verifier, err := billing.NewPolarWebhookVerifier(secret, 5*time.Minute)
	if err != nil {
		t.Fatalf("create verifier: %v", err)
	}
	raw := []byte("{\n  \"type\": \"subscription.updated\",\n  \"data\": {\"id\": \"subscription-1\"}\n}")
	headers := signedWebhookHeaders("event-1", now.Unix(), raw, key)
	headers.Set("webhook-signature", "v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= "+headers.Get("webhook-signature"))

	verified, err := verifier.Verify(headers, raw, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.ExternalEventID() != "event-1" {
		t.Fatalf("external event ID = %q", verified.ExternalEventID())
	}
	if _, err := verifier.Verify(headers, []byte(`{"type":"subscription.updated","data":{"id":"subscription-1"}}`), now); !errors.Is(err, billing.ErrInvalidPolarWebhookSignature) {
		t.Fatalf("tampered raw body error = %v", err)
	}
}

func TestPolarWebhookVerifierRejectsMalformedTamperedAndStaleRequests(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	secret, key := webhookSecret()
	verifier, err := billing.NewPolarWebhookVerifier(secret, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"type":"subscription.created","data":{}}`)
	valid := signedWebhookHeaders("event-1", now.Unix(), raw, key)
	tests := []struct {
		name    string
		headers http.Header
		want    error
	}{
		{name: "missing ID", headers: cloneHeaderWithout(valid, "webhook-id"), want: billing.ErrMalformedPolarWebhook},
		{name: "ambiguous ID", headers: signedWebhookHeaders("event.with.dot", now.Unix(), raw, key), want: billing.ErrMalformedPolarWebhook},
		{name: "malformed timestamp", headers: replaceHeader(valid, "webhook-timestamp", "yesterday"), want: billing.ErrMalformedPolarWebhook},
		{name: "malformed signature", headers: replaceHeader(valid, "webhook-signature", "v1,not-base64"), want: billing.ErrInvalidPolarWebhookSignature},
		{name: "tampered signature", headers: replaceHeader(valid, "webhook-signature", "v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="), want: billing.ErrInvalidPolarWebhookSignature},
		{name: "stale", headers: signedWebhookHeaders("event-1", now.Add(-6*time.Minute).Unix(), raw, key), want: billing.ErrStalePolarWebhook},
		{name: "too far future", headers: signedWebhookHeaders("event-1", now.Add(6*time.Minute).Unix(), raw, key), want: billing.ErrStalePolarWebhook},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := verifier.Verify(test.headers, raw, now); !errors.Is(err, test.want) {
				t.Fatalf("Verify() error = %v, want %v", err, test.want)
			}
		})
	}
}

func webhookSecret() (string, []byte) {
	key := []byte("0123456789abcdef0123456789abcdef")
	return "whsec_" + base64.StdEncoding.EncodeToString(key), key
}

func signedWebhookHeaders(id string, timestamp int64, body, key []byte) http.Header {
	timestampText := strconv.FormatInt(timestamp, 10)
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s.%s.", id, timestampText)
	_, _ = mac.Write(body)
	return http.Header{
		"Webhook-Id":        []string{id},
		"Webhook-Timestamp": []string{timestampText},
		"Webhook-Signature": []string{"v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))},
	}
}

func cloneHeaderWithout(source http.Header, name string) http.Header {
	clone := source.Clone()
	clone.Del(name)
	return clone
}

func replaceHeader(source http.Header, name, value string) http.Header {
	clone := source.Clone()
	clone.Set(name, value)
	return clone
}
