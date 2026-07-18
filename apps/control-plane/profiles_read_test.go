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
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestListProfilesHTTPReportsOwnedProfilesWithHeadVersion(t *testing.T) {
	profile := newTestProfile(t, "profile-1", "user-1", "Default", "default")
	headVersionID := "profile-version-1"
	reads := &profileReaderFake{list: []db.ProfileDetail{{Profile: profile, HeadVersionID: &headVersionID}}}
	handler := profileReadHandler(reads, []string{"request-list"})

	response := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
	}
	var page contracts.ProfilePage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("decode ProfilePage: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Id != "profile-1" || page.Items[0].HeadVersionId == nil || *page.Items[0].HeadVersionId != headVersionID {
		t.Fatalf("ProfilePage = %#v", page)
	}
}

func TestListProfilesHTTPReportsEmptyPageShape(t *testing.T) {
	reads := &profileReaderFake{}
	handler := profileReadHandler(reads, []string{"request-list"})

	response := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles")
	if response.Code != http.StatusOK || response.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("empty ProfilePage response = status:%d body:%s", response.Code, response.Body.String())
	}
}

func TestGetProfileHTTPReportsOwnedProfileAndHidesForeign(t *testing.T) {
	profile := newTestProfile(t, "profile-1", "user-1", "Default", "default")
	reads := &profileReaderFake{profiles: map[string]db.ProfileDetail{"profile-1": {Profile: profile}}}
	handler := profileReadHandler(reads, []string{"request-get", "request-foreign"})

	found := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles/profile-1")
	if found.Code != http.StatusOK {
		t.Fatalf("owned Profile status = %d, body:%s", found.Code, found.Body.String())
	}
	var body contracts.ProfileSummary
	if err := json.NewDecoder(found.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Id != "profile-1" || body.Name != "Default" || body.Slug != "default" || body.HeadVersionId != nil {
		t.Fatalf("Profile = %#v", body)
	}

	missing := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles/missing-profile")
	if missing.Code != http.StatusNotFound || !bytes.Contains(missing.Body.Bytes(), []byte(`"code":"PROFILE_NOT_FOUND"`)) {
		t.Fatalf("foreign Profile response = status:%d body:%s", missing.Code, missing.Body.String())
	}
}

func TestGetProfileVersionHTTPReportsOwnedVersionAndHidesForeign(t *testing.T) {
	version, err := domain.RestoreProfileVersion(domain.ProfileVersionSnapshot{
		ID: "profile-version-1", ProfileID: "profile-1", Version: 1,
		Digest: "sha256:" + strings.Repeat("a", 64),
		CapsuleRefs: []domain.CapsuleRef{{
			Ref: "owner/user-1/capsule@sha256:" + strings.Repeat("b", 64), FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"config:editor"},
		}},
		CreatedAt: time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("restore Profile Version: %v", err)
	}
	reads := &profileReaderFake{versions: map[string]domain.ProfileVersion{"profile-version-1": version}}
	handler := profileReadHandler(reads, []string{"request-get-version", "request-foreign"})

	found := serveProfileReadRequest(handler, http.MethodGet, "/v1/profile-versions/profile-version-1")
	if found.Code != http.StatusOK {
		t.Fatalf("owned Profile Version status = %d, body:%s", found.Code, found.Body.String())
	}
	var body contracts.ProfileVersion
	if err := json.NewDecoder(found.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Id != "profile-version-1" || body.ProfileId != "profile-1" || len(body.CapsuleRefs) != 1 {
		t.Fatalf("Profile Version = %#v", body)
	}

	missing := serveProfileReadRequest(handler, http.MethodGet, "/v1/profile-versions/missing-version")
	if missing.Code != http.StatusNotFound || !bytes.Contains(missing.Body.Bytes(), []byte(`"code":"PROFILE_VERSION_NOT_FOUND"`)) {
		t.Fatalf("foreign Profile Version response = status:%d body:%s", missing.Code, missing.Body.String())
	}
}

func newTestProfile(t *testing.T, id, ownerID, name, slug string) domain.Profile {
	t.Helper()
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: id, OwnerUserID: ownerID, Name: name, Slug: slug, CreatedAt: time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("create domain Profile: %v", err)
	}
	return profile
}

func profileReadHandler(reads controlplane.ProfileReader, requestIDs []string) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		ProfileReads: reads, Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: repeatValue("user-1", 5)},
		RequestIDs: &idsFake{values: requestIDs}, DefaultRegion: "us-east-1", Now: time.Now,
	})
}

func serveProfileReadRequest(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type profileReaderFake struct {
	profiles map[string]db.ProfileDetail
	list     []db.ProfileDetail
	versions map[string]domain.ProfileVersion
	err      error
}

func (fake *profileReaderFake) GetOwnedProfile(_ context.Context, _, profileID string) (db.ProfileDetail, error) {
	if fake.err != nil {
		return db.ProfileDetail{}, fake.err
	}
	detail, found := fake.profiles[profileID]
	if !found {
		return db.ProfileDetail{}, db.ErrReferenceNotOwned
	}
	return detail, nil
}

func (fake *profileReaderFake) ListOwnedProfiles(_ context.Context, _ string) ([]db.ProfileDetail, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	return fake.list, nil
}

func (fake *profileReaderFake) GetOwnedProfileVersion(_ context.Context, _, versionID string) (domain.ProfileVersion, error) {
	if fake.err != nil {
		return domain.ProfileVersion{}, fake.err
	}
	version, found := fake.versions[versionID]
	if !found {
		return domain.ProfileVersion{}, db.ErrReferenceNotOwned
	}
	return version, nil
}
