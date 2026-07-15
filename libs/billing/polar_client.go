package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const maxPolarResponseBytes = 64 << 10

type PolarEventClient struct {
	endpoint    string
	accessToken string
	httpClient  *http.Client
}

type PolarRetryableError struct {
	statusCode int
	cause      error
}

func (err *PolarRetryableError) Error() string {
	if err.statusCode != 0 {
		return fmt.Sprintf("deliver Polar event: retryable HTTP status %d", err.statusCode)
	}
	return "deliver Polar event: retryable transport failure"
}

func (err *PolarRetryableError) Unwrap() error { return err.cause }

func (err *PolarRetryableError) StatusCode() int { return err.statusCode }

type PolarTerminalError struct {
	statusCode int
	cause      error
}

func (err *PolarTerminalError) Error() string {
	if err.statusCode != 0 {
		return fmt.Sprintf("deliver Polar event: terminal HTTP status %d", err.statusCode)
	}
	return "deliver Polar event: terminal request failure"
}

func (err *PolarTerminalError) Unwrap() error { return err.cause }

func (err *PolarTerminalError) StatusCode() int { return err.statusCode }

func NewPolarEventClient(endpoint, accessToken string, httpClient *http.Client) (*PolarEventClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("create Polar event client: absolute HTTPS endpoint without credentials, query, or fragment is required")
	}
	if accessToken == "" || httpClient == nil {
		return nil, errors.New("create Polar event client: access token and HTTP client are required")
	}
	return &PolarEventClient{endpoint: parsed.String(), accessToken: accessToken, httpClient: httpClient}, nil
}

func (client *PolarEventClient) Deliver(ctx context.Context, event CreditsUsedEvent) error {
	if client == nil || client.httpClient == nil || client.endpoint == "" || client.accessToken == "" {
		return &PolarTerminalError{cause: errors.New("Polar event client is not initialized")}
	}
	body, err := json.Marshal(struct {
		Events [1]CreditsUsedEvent `json:"events"`
	}{Events: [1]CreditsUsedEvent{event}})
	if err != nil {
		return &PolarTerminalError{cause: err}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return &PolarTerminalError{cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+client.accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &PolarRetryableError{cause: err}
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxPolarResponseBytes))
	if response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode == http.StatusConflict {
		return nil
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return &PolarRetryableError{statusCode: response.StatusCode}
	}
	return &PolarTerminalError{statusCode: response.StatusCode}
}
