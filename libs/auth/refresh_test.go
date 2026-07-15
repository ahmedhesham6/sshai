package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRefreshClientRotatesPublicCLITokensWithoutASecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/user_management/authenticate" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		assertForm(t, request, url.Values{
			"client_id": {"client_public_01"}, "grant_type": {"refresh_token"}, "refresh_token": {"refresh_old"},
		})
		_, _ = response.Write([]byte(`{"access_token":"access_new","refresh_token":"refresh_new"}`))
	}))
	defer server.Close()
	client, err := newRefreshClient(refreshClientConfig{
		clientID: "client_public_01", endpoint: server.URL + "/user_management/authenticate", httpClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("create refresh client: %v", err)
	}
	credential, err := NewRefreshCredential("refresh_old")
	if err != nil {
		t.Fatalf("create refresh credential: %v", err)
	}
	pair, err := client.Refresh(context.Background(), credential)
	if err != nil {
		t.Fatalf("refresh tokens: %v", err)
	}
	if pair.AccessToken() != "access_new" || pair.RefreshToken() != "refresh_new" {
		t.Fatal("refresh did not return the complete rotated pair")
	}
	for _, rendered := range []string{fmt.Sprint(credential), fmt.Sprintf("%#v", credential), fmt.Sprint(pair), fmt.Sprintf("%#v", pair)} {
		if strings.Contains(rendered, "refresh_old") || strings.Contains(rendered, "access_new") || strings.Contains(rendered, "refresh_new") {
			t.Fatalf("secret-bearing value rendered a token: %q", rendered)
		}
	}
}

func TestRefreshClientClassifiesRedactedFailuresAndDeniesRedirects(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		body          string
		wantTerminal  bool
		wantRetryable bool
	}{
		{name: "expired refresh", status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, wantTerminal: true},
		{name: "incomplete rotation", status: http.StatusOK, body: `{"access_token":"access_only"}`, wantTerminal: true},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":"slow_down"}`, wantRetryable: true},
		{name: "dependency unavailable", status: http.StatusServiceUnavailable, body: `{}`, wantRetryable: true},
		{name: "redirect", status: http.StatusTemporaryRedirect, body: `{}`, wantRetryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if test.status == http.StatusTemporaryRedirect {
					response.Header().Set("Location", "/secret-in-redirect")
				}
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := newRefreshClient(refreshClientConfig{
				clientID: "client_public_01", endpoint: server.URL, httpClient: server.Client(),
			})
			if err != nil {
				t.Fatalf("create refresh client: %v", err)
			}
			credential, _ := NewRefreshCredential("DO_NOT_LEAK_REFRESH")
			_, err = client.Refresh(context.Background(), credential)
			var terminal *TerminalAuthError
			if errors.As(err, &terminal) != test.wantTerminal {
				t.Fatalf("terminal error = %v, want %t", err, test.wantTerminal)
			}
			var retryable *RetryableDependencyError
			if errors.As(err, &retryable) != test.wantRetryable {
				t.Fatalf("retryable error = %v, want %t", err, test.wantRetryable)
			}
			for _, rendered := range []string{fmt.Sprint(err), fmt.Sprintf("%#v", err)} {
				if strings.Contains(rendered, "DO_NOT_LEAK_REFRESH") {
					t.Fatalf("error rendered refresh token: %q", rendered)
				}
			}
		})
	}
}
