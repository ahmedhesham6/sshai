package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestProfileHTTPTracerCreatesAndPublishesOwnedProfile(t *testing.T) {
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

	publishedResponse := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-publish-key", `{
		"expectedHeadVersionId":null,
		"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"artifacts":[{
			"kind":"agent_instruction","sourceLocator":"AGENTS.md#$",
			"sourceDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"contentDigest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			"sizeBytes":42,"mode":416,
			"sensitivity":"private","trust":"user_authored","containsExecutable":false
		}]
	}`)
	if publishedResponse.Code != http.StatusCreated || publishedResponse.Header().Get("X-Request-ID") != "request-publish" {
		t.Fatalf("publish response = status:%d request:%q body:%s", publishedResponse.Code, publishedResponse.Header().Get("X-Request-ID"), publishedResponse.Body.String())
	}
	var published contracts.ProfileVersion
	if err := json.NewDecoder(publishedResponse.Body).Decode(&published); err != nil {
		t.Fatalf("decode published Profile Version: %v", err)
	}
	if published.Id != "version-1" || published.ProfileId != "profile-1" || published.Version != 1 || len(published.Artifacts) != 1 || published.Artifacts[0].Id != "artifact-1" || published.Artifacts[0].SizeBytes != 42 || published.Artifacts[0].Mode != 416 {
		t.Fatalf("published Profile Version = %#v", published)
	}
	if repository.ownerID != "user-1" || repository.createKey != "profile-create-key" || repository.publishKey != "profile-publish-key" {
		t.Fatalf("repository command scope = owner:%q create:%q publish:%q", repository.ownerID, repository.createKey, repository.publishKey)
	}
}

func TestProfileHTTPMapsSafeCommandErrors(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name, path, body, wantCode string
		repositoryError            error
		wantStatus                 int
	}{
		{name: "unsupported fork", path: "/v1/profiles", body: `{"name":"Fork","forkedFromVersionId":"version-1"}`, wantStatus: http.StatusUnprocessableEntity, wantCode: "PROFILE_FORK_UNSUPPORTED"},
		{name: "create conflict", path: "/v1/profiles", body: `{"name":"Personal"}`, repositoryError: db.ErrProfileConflict, wantStatus: http.StatusConflict, wantCode: "PROFILE_CONFLICT"},
		{name: "stale head", path: "/v1/profiles/profile-1/versions", body: validProfilePublicationBody(), repositoryError: domain.ErrStaleProfileHead, wantStatus: http.StatusConflict, wantCode: "STALE_PROFILE_HEAD"},
		{name: "foreign Profile", path: "/v1/profiles/profile-1/versions", body: validProfilePublicationBody(), repositoryError: db.ErrReferenceNotOwned, wantStatus: http.StatusNotFound, wantCode: "PROFILE_NOT_FOUND"},
		{name: "unavailable", path: "/v1/profiles/profile-1/versions", body: validProfilePublicationBody(), repositoryError: errors.New("postgres password=secret"), wantStatus: http.StatusServiceUnavailable, wantCode: "COMMAND_UNAVAILABLE"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repository := &profileHTTPRepositoryFake{err: scenario.repositoryError}
			handler := profileHandler(repository, []string{"generated-1", "generated-2"}, []string{"request-error"}, now)
			response := serveProfileRequest(handler, http.MethodPost, scenario.path, "profile-error-key", scenario.body)
			if response.Code != scenario.wantStatus || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+scenario.wantCode+`"`)) {
				t.Fatalf("error response = status:%d body:%s", response.Code, response.Body.String())
			}
			if response.Header().Get("X-Request-ID") != "request-error" || bytes.Contains(response.Body.Bytes(), []byte("password=secret")) {
				t.Fatalf("unsafe response = request:%q body:%s", response.Header().Get("X-Request-ID"), response.Body.String())
			}
		})
	}
}

func TestProfileHTTPMapsUploadVerificationErrorsSafely(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid upload", err: application.ErrUploadNotVerified, wantStatus: http.StatusBadRequest, wantCode: "INVALID_UPLOAD"},
		{name: "missing object", err: application.ErrUploadObjectNotFound, wantStatus: http.StatusNotFound, wantCode: "UPLOAD_NOT_FOUND"},
		{name: "unavailable", err: errors.New("s3 password=secret"), wantStatus: http.StatusServiceUnavailable, wantCode: "COMMAND_UNAVAILABLE"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			now := time.Now()
			handler := controlplane.NewHandler(controlplane.Config{
				Profiles: application.NewProfileService(&profileHTTPRepositoryFake{}, &successfulUploadVerifier{size: 42, err: scenario.err}, &idsFake{values: []string{"version-1", "artifact-1"}}, func() time.Time { return now }),
				Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
				RequestIDs: &idsFake{values: []string{"request-upload-error"}}, DefaultRegion: "us-east-1", Now: func() time.Time { return now },
			})
			response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "profile-publish-key", validProfilePublicationBody())
			if response.Code != scenario.wantStatus || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+scenario.wantCode+`"`)) || bytes.Contains(response.Body.Bytes(), []byte("password=secret")) {
				t.Fatalf("response = status:%d body:%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestProfileHTTPRejectsMissingOrOutOfRangeArtifactFilesystemMetadataBeforeApplication(t *testing.T) {
	valid := validProfilePublicationBody()
	tests := []struct{ name, body string }{
		{name: "missing size", body: strings.Replace(valid, `"sizeBytes":42,`, "", 1)},
		{name: "missing mode", body: strings.Replace(valid, `"mode":416,`, "", 1)},
		{name: "negative size", body: strings.Replace(valid, `"sizeBytes":42`, `"sizeBytes":-1`, 1)},
		{name: "negative mode", body: strings.Replace(valid, `"mode":416`, `"mode":-1`, 1)},
		{name: "mode above permissions", body: strings.Replace(valid, `"mode":416`, `"mode":512`, 1)},
		{name: "mode uint32 wrap", body: strings.Replace(valid, `"mode":416`, `"mode":4294967296`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &profileHTTPRepositoryFake{}
			handler := profileHandler(repository, []string{"unused"}, []string{"request-invalid"}, time.Now())
			response := serveProfileRequest(handler, http.MethodPost, "/v1/profiles/profile-1/versions", "key", test.body)
			if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"INVALID_REQUEST"`)) {
				t.Fatalf("response = status:%d body:%s", response.Code, response.Body.String())
			}
			if repository.publishCalls != 0 {
				t.Fatalf("invalid request reached Profile publication %d times", repository.publishCalls)
			}
		})
	}
}

func profileHandler(repository application.ProfileRepository, profileIDs, requestIDs []string, now time.Time) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		Profiles: application.NewProfileService(repository, &successfulUploadVerifier{size: 42}, &idsFake{values: profileIDs}, func() time.Time { return now }),
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1", "user-1"}},
		RequestIDs: &idsFake{values: requestIDs}, DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
}

func serveProfileRequest(handler http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func validProfilePublicationBody() string {
	return `{"expectedHeadVersionId":null,"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","artifacts":[{"kind":"agent_instruction","sourceLocator":"AGENTS.md#$","sourceDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","contentDigest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","sizeBytes":42,"mode":416,"sensitivity":"private","trust":"user_authored","containsExecutable":false}]}`
}

type profileHTTPRepositoryFake struct {
	profile            domain.Profile
	ownerID, createKey string
	publishKey         string
	publishCalls       int
	err                error
}

func (repository *profileHTTPRepositoryFake) CreateProfile(_ context.Context, profile domain.Profile, key string) (domain.Profile, error) {
	repository.profile, repository.ownerID, repository.createKey = profile, profile.Snapshot().OwnerUserID, key
	return profile, repository.err
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
