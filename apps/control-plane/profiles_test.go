package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestProfileHTTPCreatesAndPublishesOwnedProfile(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &profileHTTPRepositoryFake{}
	handler := profileHandler(repository, []string{"profile-1", "version-1", "artifact-1"}, []string{"request-create", "request-publish"}, now)

	createdResponse := serveProfileRequest(handler, http.MethodPost, "/v1/profiles", "profile-create-key", `{"name":"Personal Config"}`)
	if createdResponse.Code != http.StatusCreated || createdResponse.Header().Get("X-Request-ID") != "request-create" {
		t.Fatalf("create response = status:%d request:%q body:%s", createdResponse.Code, createdResponse.Header().Get("X-Request-ID"), createdResponse.Body.String())
	}
	var created contracts.ProfileSummary
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatalf("decode created Profile: %v", err)
	}
	if created.Id != "profile-1" || created.Name != "Personal Config" || created.Slug != "personal-config" {
		t.Fatalf("created Profile = %#v", created)
	}

	publishedResponse := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-publish-key", validProfilePublicationBody())
	if publishedResponse.Code != http.StatusCreated || publishedResponse.Header().Get("X-Request-ID") != "request-publish" {
		t.Fatalf("publish response = status:%d request:%q body:%s", publishedResponse.Code, publishedResponse.Header().Get("X-Request-ID"), publishedResponse.Body.String())
	}
	var published contracts.ProfileVersion
	if err := json.NewDecoder(publishedResponse.Body).Decode(&published); err != nil {
		t.Fatalf("decode published Profile Version: %v", err)
	}
	if published.Id != "version-1" || published.ProfileId != "profile-1" || published.Version != 1 || len(published.CapsuleRefs) != 1 {
		t.Fatalf("published Profile Version = %#v", published)
	}
	if published.Digest != domain.ComputeProfileVersionDigest([]domain.CapsuleRef{{
		Ref: "owner/user-1/capsule@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"config:editor"},
	}}) {
		t.Fatalf("published digest = %q, want server-computed digest", published.Digest)
	}
	if repository.publishCalls != 1 || repository.ownerID != "user-1" || repository.publishKey != "profile-publish-key" {
		t.Fatalf("publication repository scope = calls:%d owner:%q key:%q", repository.publishCalls, repository.ownerID, repository.publishKey)
	}
}

func TestProfileHTTPMapsSafeCreateErrors(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name, body, wantCode string
		repositoryError      error
		wantStatus           int
	}{
		{name: "unsupported fork", body: `{"name":"Fork","forkedFromVersionId":"version-1"}`, wantStatus: http.StatusUnprocessableEntity, wantCode: "PROFILE_FORK_UNSUPPORTED"},
		{name: "create conflict", body: `{"name":"Personal"}`, repositoryError: db.ErrProfileConflict, wantStatus: http.StatusConflict, wantCode: "PROFILE_CONFLICT"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repository := &profileHTTPRepositoryFake{err: scenario.repositoryError}
			handler := profileHandler(repository, []string{"generated-1", "generated-2"}, []string{"request-error"}, now)
			response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles", "profile-error-key", scenario.body)
			if response.Code != scenario.wantStatus || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+scenario.wantCode+`"`)) {
				t.Fatalf("error response = status:%d body:%s", response.Code, response.Body.String())
			}
			if response.Header().Get("X-Request-ID") != "request-error" || bytes.Contains(response.Body.Bytes(), []byte("password=secret")) {
				t.Fatalf("unsafe response = request:%q body:%s", response.Header().Get("X-Request-ID"), response.Body.String())
			}
		})
	}
}

func TestProfileHTTPRejectsInvalidCapsulePublicationContract(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
	}{
		{name: "bad freshnessPolicy", body: `{"expectedHeadVersionId":null,"capsuleRefs":[{"ref":"owner/user-1/capsule@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","freshnessPolicy":"archive"}]}`},
		{name: "malformed ref", body: `{"expectedHeadVersionId":null,"capsuleRefs":[{"ref":"not a registry reference","freshnessPolicy":"track"}]}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repository := &profileHTTPRepositoryFake{}
			handler := profileHandler(repository, []string{"unused"}, []string{"request-invalid"}, time.Now())
			response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-publish-key", scenario.body)
			if response.Code < http.StatusBadRequest || response.Code >= http.StatusInternalServerError {
				t.Fatalf("invalid publication status = %d, want 4xx; body:%s", response.Code, response.Body.String())
			}
			assertErrorResponse(t, response, "INVALID_REQUEST")
			if repository.publishCalls != 0 {
				t.Fatalf("invalid request reached Profile publication %d times", repository.publishCalls)
			}
		})
	}
}

func TestProfileHTTPRejectsForeignOwnedCapsuleRef(t *testing.T) {
	repository := &profileHTTPRepositoryFake{}
	handler := profileHandler(repository, []string{"unused"}, []string{"request-foreign-ref"}, time.Now())
	body := `{"expectedHeadVersionId":null,"capsuleRefs":[{"ref":"owner/user-2/capsule@sha256:` + strings.Repeat("a", 64) + `","freshnessPolicy":"pin"}]}`

	response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-foreign-ref-key", body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign Capsule Ref status = %d, want 422; body:%s", response.Code, response.Body.String())
	}
	assertErrorResponse(t, response, "PROFILE_INCOMPATIBLE")
	if repository.publishCalls != 0 {
		t.Fatalf("foreign Capsule Ref reached publication %d times", repository.publishCalls)
	}
}

func TestProfileHTTPPublicationAuthenticatesAndMapsConflicts(t *testing.T) {
	for _, test := range []struct {
		name          string
		withAuth      bool
		repositoryErr error
		wantStatus    int
	}{
		{name: "unauthenticated", wantStatus: http.StatusUnauthorized},
		{name: "foreign Profile", withAuth: true, repositoryErr: db.ErrReferenceNotOwned, wantStatus: http.StatusNotFound},
		{name: "owned Profile", withAuth: true, wantStatus: http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &profileHTTPRepositoryFake{err: test.repositoryErr}
			handler := profileHandler(repository, []string{"unused"}, []string{"request-publication"}, time.Now())
			response := serveProfileRequestWithAuth(handler, test.withAuth, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-publish-key", validProfilePublicationBody())
			if response.Code != test.wantStatus {
				t.Fatalf("publication status = %d, want %d; body:%s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantStatus == http.StatusCreated && repository.publishCalls != 1 {
				t.Fatalf("owned publication persistence calls = %d, want 1", repository.publishCalls)
			}
			if test.wantStatus == http.StatusNotFound && repository.publishCalls != 1 {
				t.Fatalf("foreign publication persistence calls = %d, want 1", repository.publishCalls)
			}
		})
	}
}

func TestProfileHTTPMapsStaleHeadToConflict(t *testing.T) {
	repository := &profileHTTPRepositoryFake{err: domain.ErrStaleProfileHead}
	handler := profileHandler(repository, []string{"unused"}, []string{"request-stale"}, time.Now())
	response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-publish-key", validProfilePublicationBody())
	if response.Code != http.StatusConflict {
		t.Fatalf("stale publication status = %d, want 409; body:%s", response.Code, response.Body.String())
	}
	assertErrorResponse(t, response, "STALE_PROFILE_HEAD")
}

func profileHandler(repository application.ProfileRepository, profileIDs, requestIDs []string, now time.Time) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		Profiles: application.NewProfileService(repository, &successfulUploadVerifier{size: 42}, &idsFake{values: profileIDs}, func() time.Time { return now }),
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1", "user-1"}},
		RequestIDs: &idsFake{values: requestIDs}, DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
}

func serveProfileRequest(handler http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	return serveProfileRequestWithAuth(handler, true, method, path, key, body)
}

func serveProfileRequestWithAuth(handler http.Handler, withAuth bool, method, path, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if withAuth {
		request.Header.Set("Authorization", "Bearer valid-token")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func validProfilePublicationBody() string {
	return `{"expectedHeadVersionId":null,"capsuleRefs":[{"ref":"owner/user-1/capsule@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","freshnessPolicy":"track","exclusions":["config:editor"]}]}`
}

func assertErrorResponse(t *testing.T, response *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var body contracts.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.RequestId != response.Header().Get("X-Request-ID") || body.Error.Code != wantCode || body.Error.Message == "" {
		t.Fatalf("error response = %#v, want request ID %q and code %q", body, response.Header().Get("X-Request-ID"), wantCode)
	}
}

type profileHTTPRepositoryFake struct {
	profile            domain.Profile
	ownerID, createKey string
	publishKey         string
	publishCalls       int
	ownershipCalls     int
	err                error
}

func (repository *profileHTTPRepositoryFake) CreateProfile(_ context.Context, profile domain.Profile, key string) (domain.Profile, error) {
	repository.profile, repository.ownerID, repository.createKey = profile, profile.Snapshot().OwnerUserID, key
	return profile, repository.err
}

func (repository *profileHTTPRepositoryFake) CheckProfileOwnership(_ context.Context, _, _ string) error {
	repository.ownershipCalls++
	return repository.err
}

func (repository *profileHTTPRepositoryFake) PublishProfileVersion(_ context.Context, ownerID, profileID string, expectedHead *string, publication domain.ProfileVersionPublication, key string) (domain.ProfileVersion, error) {
	repository.publishCalls++
	repository.ownerID, repository.publishKey = ownerID, key
	if repository.err != nil {
		return domain.ProfileVersion{}, repository.err
	}
	profile := repository.profile
	if profile.Snapshot().ID == "" {
		var err error
		profile, err = domain.CreateProfile(domain.ProfileSnapshot{ID: profileID, OwnerUserID: ownerID, Name: "Personal", Slug: "personal", CreatedAt: publication.CreatedAt})
		if err != nil {
			return domain.ProfileVersion{}, err
		}
	}
	return profile.PublishVersion(nil, expectedHead, publication)
}
