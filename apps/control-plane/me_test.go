package controlplane_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/contracts"
)

func TestGetCurrentUserHTTPReportsAuthenticatedUser(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	handler := controlplane.NewHandler(controlplane.Config{
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
		RequestIDs: &idsFake{values: []string{"request-me"}}, DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("X-Request-ID") != "request-me" {
		t.Fatalf("status = %d, request:%q, body:%s", response.Code, response.Header().Get("X-Request-ID"), response.Body.String())
	}
	var user contracts.User
	if err := json.NewDecoder(response.Body).Decode(&user); err != nil {
		t.Fatalf("decode User: %v", err)
	}
	if user.Id != "user-1" || user.DefaultRegion != "us-east-1" {
		t.Fatalf("User = %#v", user)
	}
}

func TestGetCurrentUserHTTPRequiresBearerAuthentication(t *testing.T) {
	handler := controlplane.NewHandler(controlplane.Config{
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
		RequestIDs: &idsFake{values: []string{"request-unauthorized"}}, DefaultRegion: "us-east-1", Now: time.Now,
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", response.Code, response.Body.String())
	}
}
