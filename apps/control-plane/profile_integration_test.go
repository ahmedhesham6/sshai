//go:build !race

package controlplane_test

import (
	"net/http"
	"testing"
	"time"
)

func TestProfileHTTPValidCapsulePublicationReturnsCreated(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &profileHTTPRepositoryFake{}
	handler := profileHandler(repository, []string{"unused"}, []string{"request-publish"}, now)

	response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-publish-key", validProfilePublicationBody())
	if response.Code != http.StatusCreated {
		t.Fatalf("valid Capsule Ref publication = status:%d body:%s, want 201", response.Code, response.Body.String())
	}
	if repository.publishCalls != 1 {
		t.Fatalf("publication repository calls = %d, want 1", repository.publishCalls)
	}
}
