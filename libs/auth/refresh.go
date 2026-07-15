package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const workOSRefreshEndpoint = "https://api.workos.com/user_management/authenticate"
const maxRefreshResponseBytes = 64 << 10

type RefreshCredential struct{ value string }

func NewRefreshCredential(value string) (RefreshCredential, error) {
	if value == "" {
		return RefreshCredential{}, errors.New("create refresh credential: token is required")
	}
	return RefreshCredential{value: value}, nil
}

func (RefreshCredential) String() string   { return "WorkOS refresh credential [redacted]" }
func (RefreshCredential) GoString() string { return "auth.RefreshCredential{[redacted]}" }

type TokenPair struct {
	accessToken  string
	refreshToken string
}

func NewTokenPair(accessToken, refreshToken string) (TokenPair, error) {
	if accessToken == "" || refreshToken == "" {
		return TokenPair{}, errors.New("create token pair: access and refresh tokens are required")
	}
	return TokenPair{accessToken: accessToken, refreshToken: refreshToken}, nil
}

func (pair TokenPair) AccessToken() string  { return pair.accessToken }
func (pair TokenPair) RefreshToken() string { return pair.refreshToken }
func (TokenPair) String() string            { return "WorkOS token pair [redacted]" }
func (TokenPair) GoString() string          { return "auth.TokenPair{[redacted]}" }

type TerminalAuthError struct{}

func (err *TerminalAuthError) Error() string {
	return "WorkOS session cannot be refreshed; login is required"
}

func (err *TerminalAuthError) GoString() string { return "auth.TerminalAuthError{[redacted]}" }

type RetryableDependencyError struct{ cause error }

func (err *RetryableDependencyError) Error() string {
	return "WorkOS authentication dependency is temporarily unavailable"
}

func (err *RetryableDependencyError) Unwrap() error { return err.cause }
func (err *RetryableDependencyError) GoString() string {
	return "auth.RetryableDependencyError{[redacted]}"
}

type RefreshClient struct {
	clientID string
	endpoint string
	client   *http.Client
}

type refreshClientConfig struct {
	clientID   string
	endpoint   string
	httpClient *http.Client
}

func NewRefreshClient(clientID string) (*RefreshClient, error) {
	return newRefreshClient(refreshClientConfig{clientID: clientID, endpoint: workOSRefreshEndpoint})
}

func newRefreshClient(config refreshClientConfig) (*RefreshClient, error) {
	if config.clientID == "" {
		return nil, errors.New("create WorkOS refresh client: public client ID is required")
	}
	endpoint, err := url.Parse(config.endpoint)
	if err != nil || !endpoint.IsAbs() || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("create WorkOS refresh client: absolute endpoint without credentials, query, or fragment is required")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	if config.httpClient != nil {
		copy := *config.httpClient
		client = &copy
		if client.Timeout <= 0 {
			client.Timeout = 15 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &RefreshClient{clientID: config.clientID, endpoint: endpoint.String(), client: client}, nil
}

func (client *RefreshClient) Refresh(ctx context.Context, credential RefreshCredential) (TokenPair, error) {
	if client == nil || client.client == nil || client.clientID == "" || client.endpoint == "" || credential.value == "" {
		return TokenPair{}, &TerminalAuthError{}
	}
	form := url.Values{
		"client_id": {client.clientID}, "grant_type": {"refresh_token"}, "refresh_token": {credential.value},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenPair{}, &TerminalAuthError{}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return TokenPair{}, context.Cause(ctx)
		}
		return TokenPair{}, &RetryableDependencyError{cause: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRefreshResponseBytes+1))
	if err != nil {
		return TokenPair{}, &RetryableDependencyError{cause: err}
	}
	if len(body) > maxRefreshResponseBytes {
		return TokenPair{}, &TerminalAuthError{}
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	decodeErr := json.Unmarshal(body, &payload)
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if decodeErr != nil || payload.AccessToken == "" || payload.RefreshToken == "" {
			return TokenPair{}, &TerminalAuthError{}
		}
		return NewTokenPair(payload.AccessToken, payload.RefreshToken)
	}
	if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return TokenPair{}, &TerminalAuthError{}
	}
	return TokenPair{}, &RetryableDependencyError{cause: fmt.Errorf("WorkOS refresh HTTP status %d", response.StatusCode)}
}
