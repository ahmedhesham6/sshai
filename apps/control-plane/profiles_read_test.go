package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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
	reads := &profileReaderFake{expectedOwnerID: "user-1", list: []db.ProfileDetail{{Profile: profile, HeadVersionID: &headVersionID}}}
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
	reads := &profileReaderFake{expectedOwnerID: "user-1"}
	handler := profileReadHandler(reads, []string{"request-list"})

	response := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles")
	if response.Code != http.StatusOK || response.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("empty ProfilePage response = status:%d body:%s", response.Code, response.Body.String())
	}
}

func TestListProfilesHTTPPagesWithoutOverlapOrGapsAndIsStable(t *testing.T) {
	base := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	list := make([]db.ProfileDetail, 0, 5)
	for index := 0; index < 5; index++ {
		id := fmt.Sprintf("profile-%d", index+1)
		profile, err := domain.CreateProfile(domain.ProfileSnapshot{
			ID: id, OwnerUserID: "user-1", Name: id, Slug: id, CreatedAt: base.Add(time.Duration(index) * time.Minute),
		})
		if err != nil {
			t.Fatalf("create Profile %d: %v", index, err)
		}
		list = append(list, db.ProfileDetail{Profile: profile})
	}
	reads := &profileReaderFake{expectedOwnerID: "user-1", list: list}
	handler := profileReadHandler(reads, repeatValue("request", 10))

	first := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles?pageSize=2")
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, body:%s", first.Code, first.Body.String())
	}
	var firstPage contracts.ProfilePage
	if err := json.NewDecoder(first.Body).Decode(&firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 2 || firstPage.NextCursor == nil {
		t.Fatalf("first ProfilePage = %#v", firstPage)
	}

	// Replaying the exact same request must reproduce the identical first
	// page: pagination is stable under identical calls.
	replay := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles?pageSize=2")
	var replayPage contracts.ProfilePage
	if err := json.NewDecoder(replay.Body).Decode(&replayPage); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstPage, replayPage) {
		t.Fatalf("replayed first ProfilePage = %#v, want identical to %#v", replayPage, firstPage)
	}

	second := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles?pageSize=2&cursor="+url.QueryEscape(*firstPage.NextCursor))
	if second.Code != http.StatusOK {
		t.Fatalf("second page status = %d, body:%s", second.Code, second.Body.String())
	}
	var secondPage contracts.ProfilePage
	if err := json.NewDecoder(second.Body).Decode(&secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 2 || secondPage.NextCursor == nil {
		t.Fatalf("second ProfilePage = %#v", secondPage)
	}

	third := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles?pageSize=2&cursor="+url.QueryEscape(*secondPage.NextCursor))
	if third.Code != http.StatusOK {
		t.Fatalf("third page status = %d, body:%s", third.Code, third.Body.String())
	}
	var thirdPage contracts.ProfilePage
	if err := json.NewDecoder(third.Body).Decode(&thirdPage); err != nil {
		t.Fatal(err)
	}
	if len(thirdPage.Items) != 1 || thirdPage.NextCursor != nil {
		t.Fatalf("third (final) ProfilePage = %#v, want 1 item and no next cursor", thirdPage)
	}

	seen := map[string]bool{}
	var order []string
	for _, page := range []contracts.ProfilePage{firstPage, secondPage, thirdPage} {
		for _, item := range page.Items {
			if seen[item.Id] {
				t.Fatalf("Profile %q returned on more than one page: overlap across %#v", item.Id, order)
			}
			seen[item.Id] = true
			order = append(order, item.Id)
		}
	}
	want := []string{"profile-1", "profile-2", "profile-3", "profile-4", "profile-5"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("paged Profile order = %#v, want %#v (no gaps, stable order)", order, want)
	}
}

func TestListProfilesHTTPRejectsInvalidCursor(t *testing.T) {
	reads := &profileReaderFake{expectedOwnerID: "user-1"}
	handler := profileReadHandler(reads, []string{"request-invalid-cursor"})

	response := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles?cursor=not-a-valid-cursor")
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"INVALID_CURSOR"`)) {
		t.Fatalf("invalid cursor response = status:%d body:%s", response.Code, response.Body.String())
	}
	var body contracts.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode ErrorResponse: %v", err)
	}
	if body.RequestId == "" || body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("ErrorResponse = %#v, want the contract's requestId/error.code/error.message populated", body)
	}
}

func TestListProfilesHTTPClampsPageSizeToContractMaximum(t *testing.T) {
	base := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	list := make([]db.ProfileDetail, 0, 150)
	for index := 0; index < 150; index++ {
		id := fmt.Sprintf("profile-%03d", index)
		snapshot := domain.ProfileSnapshot{ID: id, OwnerUserID: "user-1", Name: id, Slug: id, CreatedAt: base.Add(time.Duration(index) * time.Second)}
		profile, err := domain.CreateProfile(snapshot)
		if err != nil {
			t.Fatalf("create Profile %d: %v", index, err)
		}
		list = append(list, db.ProfileDetail{Profile: profile})
	}
	reads := &profileReaderFake{expectedOwnerID: "user-1", list: list}
	handler := profileReadHandler(reads, []string{"request-max-page-size"})

	// pageSize=100 is the contract's declared maximum (api/openapi.yaml
	// components.parameters.PageSize); with 150 owned Profiles the response
	// must be clamped to exactly 100 items and report a next cursor.
	response := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles?pageSize=100")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
	}
	var page contracts.ProfilePage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 100 || page.NextCursor == nil {
		t.Fatalf("ProfilePage item count = %d, want 100 with a next cursor", len(page.Items))
	}
}

func TestListProfilesHTTPUsesDefaultPageSizeWhenOmitted(t *testing.T) {
	base := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	list := make([]db.ProfileDetail, 0, 60)
	for index := 0; index < 60; index++ {
		id := fmt.Sprintf("profile-%03d", index)
		snapshot := domain.ProfileSnapshot{ID: id, OwnerUserID: "user-1", Name: id, Slug: id, CreatedAt: base.Add(time.Duration(index) * time.Second)}
		profile, err := domain.CreateProfile(snapshot)
		if err != nil {
			t.Fatalf("create Profile %d: %v", index, err)
		}
		list = append(list, db.ProfileDetail{Profile: profile})
	}
	reads := &profileReaderFake{expectedOwnerID: "user-1", list: list}
	handler := profileReadHandler(reads, []string{"request-default-page-size"})

	// No pageSize declared: api/openapi.yaml defaults PageSize to 50, so 60
	// owned Profiles must still be split across two pages.
	response := serveProfileReadRequest(handler, http.MethodGet, "/v1/profiles")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
	}
	var page contracts.ProfilePage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 50 || page.NextCursor == nil {
		t.Fatalf("default-page-size ProfilePage item count = %d, want 50 with a next cursor", len(page.Items))
	}
}

func TestGetProfileHTTPReportsOwnedProfileAndHidesForeign(t *testing.T) {
	profile := newTestProfile(t, "profile-1", "user-1", "Default", "default")
	reads := &profileReaderFake{expectedOwnerID: "user-1", profiles: map[string]db.ProfileDetail{"profile-1": {Profile: profile}}}
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
	reads := &profileReaderFake{expectedOwnerID: "user-1", versions: map[string]domain.ProfileVersion{"profile-version-1": version}}
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
	expectedOwnerID string
	profiles        map[string]db.ProfileDetail
	list            []db.ProfileDetail
	versions        map[string]domain.ProfileVersion
	err             error
}

func (fake *profileReaderFake) GetOwnedProfile(_ context.Context, ownerID, profileID string) (db.ProfileDetail, error) {
	if err := requireOwner("GetOwnedProfile", ownerID, fake.expectedOwnerID); err != nil {
		return db.ProfileDetail{}, err
	}
	if fake.err != nil {
		return db.ProfileDetail{}, fake.err
	}
	detail, found := fake.profiles[profileID]
	if !found {
		return db.ProfileDetail{}, db.ErrReferenceNotOwned
	}
	return detail, nil
}

func (fake *profileReaderFake) ListOwnedProfiles(_ context.Context, ownerID string, cursor *db.Cursor, pageSize int) ([]db.ProfileDetail, *db.Cursor, error) {
	if err := requireOwner("ListOwnedProfiles", ownerID, fake.expectedOwnerID); err != nil {
		return nil, nil, err
	}
	if fake.err != nil {
		return nil, nil, fake.err
	}
	items, next := paginateFake(fake.list, func(detail db.ProfileDetail) (time.Time, string) {
		snapshot := detail.Profile.Snapshot()
		return snapshot.CreatedAt, snapshot.ID
	}, cursor, pageSize)
	return items, next, nil
}

func (fake *profileReaderFake) GetOwnedProfileVersion(_ context.Context, ownerID, versionID string) (domain.ProfileVersion, error) {
	if err := requireOwner("GetOwnedProfileVersion", ownerID, fake.expectedOwnerID); err != nil {
		return domain.ProfileVersion{}, err
	}
	if fake.err != nil {
		return domain.ProfileVersion{}, fake.err
	}
	version, found := fake.versions[versionID]
	if !found {
		return domain.ProfileVersion{}, db.ErrReferenceNotOwned
	}
	return version, nil
}
