package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDeviceFlowStartsAuthorizationWithoutAClientSecret(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/user_management/authorize/device" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		assertForm(t, request, url.Values{"client_id": {"client-1"}})
		_ = json.NewEncoder(response).Encode(map[string]any{
			"device_code": "device-secret", "user_code": "ABCD-EFGH",
			"verification_uri":          "https://auth.example/device",
			"verification_uri_complete": "https://auth.example/device?user_code=ABCD-EFGH",
			"expires_in":                300, "interval": 5,
		})
	}))
	defer server.Close()
	flow, err := newDeviceFlow(deviceFlowConfig{
		clientID: "client-1", apiURL: server.URL, httpClient: server.Client(), now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDeviceFlow(): %v", err)
	}

	authorization, err := flow.Authorize(t.Context())
	if err != nil {
		t.Fatalf("Authorize(): %v", err)
	}
	if authorization.UserCode() != "ABCD-EFGH" {
		t.Fatalf("user code = %q", authorization.UserCode())
	}
	if authorization.VerificationURIComplete() != "https://auth.example/device?user_code=ABCD-EFGH" {
		t.Fatalf("verification URI = %q", authorization.VerificationURIComplete())
	}
	if !authorization.expiresAt.Equal(now.Add(5*time.Minute)) || authorization.interval != 5*time.Second {
		t.Fatalf("authorization timing = expires:%s interval:%s", authorization.expiresAt, authorization.interval)
	}
}

func TestDeviceFlowPollsPendingAndSlowsDownBeforeReturningTokens(t *testing.T) {
	responses := []struct {
		status int
		body   map[string]string
	}{
		{status: http.StatusBadRequest, body: map[string]string{"error": "authorization_pending"}},
		{status: http.StatusBadRequest, body: map[string]string{"error": "slow_down"}},
		{status: http.StatusOK, body: map[string]string{"access_token": "access", "refresh_token": "refresh"}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/user_management/authorize/device" {
			assertForm(t, request, url.Values{"client_id": {"client-1"}})
			_ = json.NewEncoder(response).Encode(map[string]any{
				"device_code": "device-secret", "user_code": "ABCD-EFGH",
				"verification_uri":          "https://auth.example/device",
				"verification_uri_complete": "https://auth.example/device?user_code=ABCD-EFGH",
				"expires_in":                60, "interval": 2,
			})
			return
		}
		assertForm(t, request, url.Values{
			"client_id": {"client-1"}, "device_code": {"device-secret"},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		})
		current := responses[0]
		responses = responses[1:]
		response.WriteHeader(current.status)
		_ = json.NewEncoder(response).Encode(current.body)
	}))
	defer server.Close()
	var waits []time.Duration
	flow, err := newDeviceFlow(deviceFlowConfig{
		clientID: "client-1", apiURL: server.URL, httpClient: server.Client(), now: time.Now,
		wait: func(_ context.Context, duration time.Duration) error {
			waits = append(waits, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewDeviceFlow(): %v", err)
	}
	authorization, err := flow.Authorize(t.Context())
	if err != nil {
		t.Fatalf("Authorize(): %v", err)
	}

	tokens, err := flow.Poll(t.Context(), authorization)
	if err != nil {
		t.Fatalf("Poll(): %v", err)
	}
	if tokens.AccessToken() != "access" || tokens.RefreshToken() != "refresh" {
		t.Fatal("tokens do not match the WorkOS response")
	}
	if len(waits) != 2 || waits[0] != 2*time.Second || waits[1] != 7*time.Second {
		t.Fatalf("poll waits = %v", waits)
	}
}

func TestDeviceFlowDefaultsPollingAndRedactsCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{
			"device_code": "device-secret", "user_code": "ABCD-EFGH",
			"verification_uri":          "https://auth.example/device",
			"verification_uri_complete": "https://auth.example/device?user_code=ABCD-EFGH",
			"expires_in":                300,
		})
	}))
	defer server.Close()
	flow, err := newDeviceFlow(deviceFlowConfig{clientID: "client-1", apiURL: server.URL, httpClient: server.Client()})
	if err != nil {
		t.Fatalf("newDeviceFlow(): %v", err)
	}
	authorization, err := flow.Authorize(t.Context())
	if err != nil {
		t.Fatalf("Authorize(): %v", err)
	}
	if authorization.interval != 5*time.Second {
		t.Fatalf("default polling interval = %s", authorization.interval)
	}
	if got := fmt.Sprintf("%#v", authorization); strings.Contains(got, "device-secret") {
		t.Fatal("authorization formatting exposed device credential")
	}
	tokens := DeviceTokens{accessToken: "access-secret", refreshToken: "refresh-secret"}
	if got := fmt.Sprintf("%#v", tokens); strings.Contains(got, "access-secret") || strings.Contains(got, "refresh-secret") {
		t.Fatal("token formatting exposed credentials")
	}
}

func TestDeviceFlowProductionConstructorPinsWorkOSTrustBoundary(t *testing.T) {
	flow, err := NewDeviceFlow("client-1")
	if err != nil {
		t.Fatalf("NewDeviceFlow(): %v", err)
	}
	if flow.apiURL != "https://api.workos.com" {
		t.Fatalf("production API URL = %q", flow.apiURL)
	}
	if err := flow.client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error = %v", err)
	}
}

func TestDeviceFlowStopsOnTerminalConditions(t *testing.T) {
	for _, workOSError := range []string{"access_denied", "expired_token"} {
		t.Run(workOSError, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/user_management/authorize/device" {
					_ = json.NewEncoder(response).Encode(validAuthorizationResponse(60, 1))
					return
				}
				requests++
				response.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(response).Encode(map[string]string{"error": workOSError})
			}))
			defer server.Close()
			flow := mustDeviceFlow(t, deviceFlowConfig{clientID: "client-1", apiURL: server.URL, httpClient: server.Client()})
			authorization := mustAuthorizeDevice(t, flow)
			if _, err := flow.Poll(t.Context(), authorization); err == nil || requests != 1 {
				t.Fatalf("Poll(): requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestDeviceFlowExpiresLocallyAndHonorsCancellation(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	tokenRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/user_management/authorize/device" {
			_ = json.NewEncoder(response).Encode(validAuthorizationResponse(1, 1))
			return
		}
		tokenRequests++
		response.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(response).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer server.Close()
	flow := mustDeviceFlow(t, deviceFlowConfig{
		clientID: "client-1", apiURL: server.URL, httpClient: server.Client(), now: func() time.Time { return now },
		wait: func(context.Context, time.Duration) error { return context.Canceled },
	})
	authorization := mustAuthorizeDevice(t, flow)
	if _, err := flow.Poll(t.Context(), authorization); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Poll() error = %v", err)
	}
	now = now.Add(time.Second)
	if _, err := flow.Poll(t.Context(), authorization); err == nil || tokenRequests != 1 {
		t.Fatalf("expired Poll(): token requests=%d err=%v", tokenRequests, err)
	}
}

func TestDeviceFlowRejectsMalformedAndIncompleteResponses(t *testing.T) {
	t.Run("malformed authorization", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, _ = response.Write([]byte("not-json"))
		}))
		defer server.Close()
		flow := mustDeviceFlow(t, deviceFlowConfig{clientID: "client-1", apiURL: server.URL, httpClient: server.Client()})
		if _, err := flow.Authorize(t.Context()); err == nil {
			t.Fatal("malformed authorization response was accepted")
		}
	})

	t.Run("incomplete tokens", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/user_management/authorize/device" {
				_ = json.NewEncoder(response).Encode(validAuthorizationResponse(60, 1))
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]string{"access_token": "access"})
		}))
		defer server.Close()
		flow := mustDeviceFlow(t, deviceFlowConfig{clientID: "client-1", apiURL: server.URL, httpClient: server.Client()})
		authorization := mustAuthorizeDevice(t, flow)
		if _, err := flow.Poll(t.Context(), authorization); err == nil {
			t.Fatal("incomplete token response was accepted")
		}
	})
}

func TestDeviceFlowCancelsInFlightTokenRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/user_management/authorize/device" {
			_ = json.NewEncoder(response).Encode(validAuthorizationResponse(60, 1))
			return
		}
		close(requestStarted)
		<-releaseRequest
	}))
	defer server.Close()
	flow := mustDeviceFlow(t, deviceFlowConfig{clientID: "client-1", apiURL: server.URL, httpClient: server.Client()})
	authorization := mustAuthorizeDevice(t, flow)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := flow.Poll(ctx, authorization)
		result <- err
	}()
	<-requestStarted
	cancel()
	err := <-result
	close(releaseRequest)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation error = %v", err)
	}
}

func validAuthorizationResponse(expiresIn, interval int) map[string]any {
	return map[string]any{
		"device_code": "device-secret", "user_code": "ABCD-EFGH",
		"verification_uri":          "https://auth.example/device",
		"verification_uri_complete": "https://auth.example/device?user_code=ABCD-EFGH",
		"expires_in":                expiresIn, "interval": interval,
	}
}

func mustDeviceFlow(t *testing.T, config deviceFlowConfig) *DeviceFlow {
	t.Helper()
	flow, err := newDeviceFlow(config)
	if err != nil {
		t.Fatalf("newDeviceFlow(): %v", err)
	}
	return flow
}

func mustAuthorizeDevice(t *testing.T, flow *DeviceFlow) DeviceAuthorization {
	t.Helper()
	authorization, err := flow.Authorize(t.Context())
	if err != nil {
		t.Fatalf("Authorize(): %v", err)
	}
	return authorization
}

func assertForm(t *testing.T, request *http.Request, want url.Values) {
	t.Helper()
	if request.Method != http.MethodPost {
		t.Fatalf("method = %q", request.Method)
	}
	if got := request.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q", got)
	}
	if err := request.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	if request.PostForm.Encode() != want.Encode() {
		t.Fatal("form fields do not match")
	}
}
