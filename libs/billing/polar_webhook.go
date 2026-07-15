package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	ErrMalformedPolarWebhook        = errors.New("malformed Polar webhook")
	ErrInvalidPolarWebhookSignature = errors.New("invalid Polar webhook signature")
	ErrStalePolarWebhook            = errors.New("stale Polar webhook")
)

type PolarWebhookVerifier struct {
	secret    []byte
	tolerance time.Duration
}

type VerifiedPolarWebhook struct {
	externalEventID string
	rawBody         []byte
}

func NewPolarWebhookVerifier(secret string, tolerance time.Duration) (*PolarWebhookVerifier, error) {
	if !strings.HasPrefix(secret, "whsec_") || tolerance <= 0 {
		return nil, errors.New("create Polar webhook verifier: whsec_ secret and positive timestamp tolerance are required")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(decoded) < 24 || len(decoded) > 64 {
		return nil, errors.New("create Polar webhook verifier: signing secret must encode 24 to 64 bytes")
	}
	return &PolarWebhookVerifier{secret: decoded, tolerance: tolerance}, nil
}

func (verifier *PolarWebhookVerifier) Verify(headers http.Header, rawBody []byte, receivedAt time.Time) (VerifiedPolarWebhook, error) {
	if verifier == nil || len(verifier.secret) == 0 || verifier.tolerance <= 0 || receivedAt.IsZero() {
		return VerifiedPolarWebhook{}, fmt.Errorf("%w: verifier or receive time is not initialized", ErrMalformedPolarWebhook)
	}
	id := headers.Get("webhook-id")
	timestampText := headers.Get("webhook-timestamp")
	signatures := headers.Get("webhook-signature")
	if id == "" || strings.Contains(id, ".") || timestampText == "" || signatures == "" || len(rawBody) == 0 {
		return VerifiedPolarWebhook{}, ErrMalformedPolarWebhook
	}
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		return VerifiedPolarWebhook{}, ErrMalformedPolarWebhook
	}
	signedAt := time.Unix(timestamp, 0)
	if signedAt.Before(receivedAt.Add(-verifier.tolerance)) || signedAt.After(receivedAt.Add(verifier.tolerance)) {
		return VerifiedPolarWebhook{}, ErrStalePolarWebhook
	}
	mac := hmac.New(sha256.New, verifier.secret)
	_, _ = fmt.Fprintf(mac, "%s.%s.", id, timestampText)
	_, _ = mac.Write(rawBody)
	want := mac.Sum(nil)
	for _, candidate := range strings.Fields(signatures) {
		version, encoded, found := strings.Cut(candidate, ",")
		if !found || version != "v1" {
			continue
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr == nil && hmac.Equal(want, decoded) {
			return VerifiedPolarWebhook{externalEventID: id, rawBody: append([]byte(nil), rawBody...)}, nil
		}
	}
	return VerifiedPolarWebhook{}, ErrInvalidPolarWebhookSignature
}

func (webhook VerifiedPolarWebhook) ExternalEventID() string { return webhook.externalEventID }
