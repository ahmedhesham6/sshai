package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestListEnvironmentsHTTPReportsOwnedEnvironments(t *testing.T) {
	detail := newTestEnvironmentDetail(t, "environment-1", "user-1")
	reads := &environmentReaderFake{list: []db.EnvironmentDetail{detail}}
	handler := environmentReadHandler(reads, []string{"request-list"})

	response := serveEnvironmentReadRequest(handler, "/v1/environments")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
	}
	var page contracts.EnvironmentPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Id != "environment-1" {
		t.Fatalf("EnvironmentPage = %#v", page)
	}
	if page.Items[0].CapsuleLockId != "" {
		t.Fatalf("CapsuleLockId = %q, want empty before resolve", page.Items[0].CapsuleLockId)
	}
	if page.Items[0].Runtime != nil {
		t.Fatalf("Runtime = %#v, want nil before provisioning", page.Items[0].Runtime)
	}
}

func TestListEnvironmentsHTTPReportsEmptyPageShape(t *testing.T) {
	reads := &environmentReaderFake{}
	handler := environmentReadHandler(reads, []string{"request-list"})

	response := serveEnvironmentReadRequest(handler, "/v1/environments")
	if response.Code != http.StatusOK || response.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("empty EnvironmentPage response = status:%d body:%s", response.Code, response.Body.String())
	}
}

func TestGetEnvironmentHTTPReportsOwnedEnvironmentAndHidesForeign(t *testing.T) {
	detail := newTestEnvironmentDetail(t, "environment-1", "user-1")
	activeOperationID := "operation-1"
	detail.ActiveOperationID = &activeOperationID
	reads := &environmentReaderFake{environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
	handler := environmentReadHandler(reads, []string{"request-get", "request-foreign"})

	found := serveEnvironmentReadRequest(handler, "/v1/environments/environment-1")
	if found.Code != http.StatusOK {
		t.Fatalf("owned Environment status = %d, body:%s", found.Code, found.Body.String())
	}
	var body contracts.Environment
	if err := json.NewDecoder(found.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Id != "environment-1" || body.ActiveOperationId == nil || *body.ActiveOperationId != "operation-1" {
		t.Fatalf("Environment = %#v", body)
	}

	missing := serveEnvironmentReadRequest(handler, "/v1/environments/missing-environment")
	if missing.Code != http.StatusNotFound || !bytes.Contains(missing.Body.Bytes(), []byte(`"code":"ENVIRONMENT_NOT_FOUND"`)) {
		t.Fatalf("foreign Environment response = status:%d body:%s", missing.Code, missing.Body.String())
	}
}

func TestListEnvironmentEventsHTTPReportsOwnedTimelineAndHidesForeign(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	operationID := "operation-1"
	reads := &environmentReaderFake{events: map[string][]db.EnvironmentEvent{
		"environment-1": {{ID: "operation-1", EnvironmentID: "environment-1", OperationID: &operationID, Type: "environment.create", Summary: "environment.create queued", CreatedAt: createdAt}},
	}}
	handler := environmentReadHandler(reads, []string{"request-events", "request-foreign"})

	found := serveEnvironmentReadRequest(handler, "/v1/environments/environment-1/events")
	if found.Code != http.StatusOK {
		t.Fatalf("owned events status = %d, body:%s", found.Code, found.Body.String())
	}
	var page contracts.EnvironmentEventPage
	if err := json.NewDecoder(found.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Id != "operation-1" || page.Items[0].OperationId == nil || *page.Items[0].OperationId != "operation-1" {
		t.Fatalf("EnvironmentEventPage = %#v", page)
	}

	missing := serveEnvironmentReadRequest(handler, "/v1/environments/missing-environment/events")
	if missing.Code != http.StatusNotFound || !bytes.Contains(missing.Body.Bytes(), []byte(`"code":"ENVIRONMENT_NOT_FOUND"`)) {
		t.Fatalf("foreign Environment events response = status:%d body:%s", missing.Code, missing.Body.String())
	}
}

func newTestEnvironmentDetail(t *testing.T, id, ownerID string) db.EnvironmentDetail {
	t.Helper()
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	environment, err := domain.RestoreEnvironment(domain.EnvironmentSnapshot{
		ID: id, OwnerUserID: ownerID, Name: "Workspace", Slug: "workspace",
		Lifecycle: domain.EnvironmentCreating, Health: domain.EnvironmentHealthUnknown,
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-version-1", UpgradePolicy: domain.UpgradeManual,
		AutoStopPolicyID: "policy-1", CreatedAt: createdAt, UpdatedAt: createdAt, Version: 1,
	})
	if err != nil {
		t.Fatalf("restore domain Environment: %v", err)
	}
	return db.EnvironmentDetail{Environment: environment, AutoStopMode: domain.AutoStopManual, GracePeriodSeconds: 0}
}

func environmentReadHandler(reads controlplane.EnvironmentReader, requestIDs []string) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		EnvironmentReads: reads, Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: repeatValue("user-1", 5)},
		RequestIDs: &idsFake{values: requestIDs}, DefaultRegion: "us-east-1", Now: time.Now,
	})
}

func serveEnvironmentReadRequest(handler http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type environmentReaderFake struct {
	environments map[string]db.EnvironmentDetail
	list         []db.EnvironmentDetail
	events       map[string][]db.EnvironmentEvent
	err          error
}

func (fake *environmentReaderFake) GetOwnedEnvironment(_ context.Context, _, environmentID string) (db.EnvironmentDetail, error) {
	if fake.err != nil {
		return db.EnvironmentDetail{}, fake.err
	}
	detail, found := fake.environments[environmentID]
	if !found {
		return db.EnvironmentDetail{}, db.ErrReferenceNotOwned
	}
	return detail, nil
}

func (fake *environmentReaderFake) ListOwnedEnvironments(_ context.Context, _ string) ([]db.EnvironmentDetail, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	return fake.list, nil
}

func (fake *environmentReaderFake) ListOwnedEnvironmentEvents(_ context.Context, _, environmentID string) ([]db.EnvironmentEvent, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	events, found := fake.events[environmentID]
	if !found {
		return nil, db.ErrReferenceNotOwned
	}
	return events, nil
}
