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
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestListEnvironmentsHTTPReportsOwnedEnvironments(t *testing.T) {
	detail := newTestEnvironmentDetail(t, "environment-1", "user-1")
	reads := &environmentReaderFake{expectedOwnerID: "user-1", list: []db.EnvironmentDetail{detail}}
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
	reads := &environmentReaderFake{expectedOwnerID: "user-1"}
	handler := environmentReadHandler(reads, []string{"request-list"})

	response := serveEnvironmentReadRequest(handler, "/v1/environments")
	if response.Code != http.StatusOK || response.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("empty EnvironmentPage response = status:%d body:%s", response.Code, response.Body.String())
	}
}

func TestListEnvironmentsHTTPPagesWithoutOverlapOrGapsAndIsStable(t *testing.T) {
	base := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	list := make([]db.EnvironmentDetail, 0, 5)
	for index := 0; index < 5; index++ {
		id := fmt.Sprintf("environment-%d", index+1)
		list = append(list, newTestEnvironmentDetailAt(t, id, "user-1", base.Add(time.Duration(index)*time.Minute)))
	}
	reads := &environmentReaderFake{expectedOwnerID: "user-1", list: list}
	handler := environmentReadHandler(reads, repeatValue("request", 10))

	first := serveEnvironmentReadRequest(handler, "/v1/environments?pageSize=2")
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, body:%s", first.Code, first.Body.String())
	}
	var firstPage contracts.EnvironmentPage
	if err := json.NewDecoder(first.Body).Decode(&firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 2 || firstPage.NextCursor == nil {
		t.Fatalf("first EnvironmentPage = %#v", firstPage)
	}

	replay := serveEnvironmentReadRequest(handler, "/v1/environments?pageSize=2")
	var replayPage contracts.EnvironmentPage
	if err := json.NewDecoder(replay.Body).Decode(&replayPage); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstPage, replayPage) {
		t.Fatalf("replayed first EnvironmentPage = %#v, want identical to %#v", replayPage, firstPage)
	}

	second := serveEnvironmentReadRequest(handler, "/v1/environments?pageSize=2&cursor="+url.QueryEscape(*firstPage.NextCursor))
	var secondPage contracts.EnvironmentPage
	if err := json.NewDecoder(second.Body).Decode(&secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 2 || secondPage.NextCursor == nil {
		t.Fatalf("second EnvironmentPage = %#v", secondPage)
	}

	third := serveEnvironmentReadRequest(handler, "/v1/environments?pageSize=2&cursor="+url.QueryEscape(*secondPage.NextCursor))
	var thirdPage contracts.EnvironmentPage
	if err := json.NewDecoder(third.Body).Decode(&thirdPage); err != nil {
		t.Fatal(err)
	}
	if len(thirdPage.Items) != 1 || thirdPage.NextCursor != nil {
		t.Fatalf("third (final) EnvironmentPage = %#v, want 1 item and no next cursor", thirdPage)
	}

	seen := map[string]bool{}
	var order []string
	for _, page := range []contracts.EnvironmentPage{firstPage, secondPage, thirdPage} {
		for _, item := range page.Items {
			if seen[item.Id] {
				t.Fatalf("Environment %q returned on more than one page: overlap across %#v", item.Id, order)
			}
			seen[item.Id] = true
			order = append(order, item.Id)
		}
	}
	want := []string{"environment-1", "environment-2", "environment-3", "environment-4", "environment-5"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("paged Environment order = %#v, want %#v (no gaps, stable order)", order, want)
	}
}

func TestListEnvironmentsHTTPRejectsInvalidCursor(t *testing.T) {
	reads := &environmentReaderFake{expectedOwnerID: "user-1"}
	handler := environmentReadHandler(reads, []string{"request-invalid-cursor"})

	response := serveEnvironmentReadRequest(handler, "/v1/environments?cursor=not-a-valid-cursor")
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"INVALID_CURSOR"`)) {
		t.Fatalf("invalid cursor response = status:%d body:%s", response.Code, response.Body.String())
	}
}

func TestListEnvironmentsHTTPClampsPageSizeToContractMaximum(t *testing.T) {
	base := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	list := make([]db.EnvironmentDetail, 0, 150)
	for index := 0; index < 150; index++ {
		id := fmt.Sprintf("environment-%03d", index)
		list = append(list, newTestEnvironmentDetailAt(t, id, "user-1", base.Add(time.Duration(index)*time.Second)))
	}
	reads := &environmentReaderFake{expectedOwnerID: "user-1", list: list}
	handler := environmentReadHandler(reads, []string{"request-max-page-size"})

	response := serveEnvironmentReadRequest(handler, "/v1/environments?pageSize=100")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
	}
	var page contracts.EnvironmentPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 100 || page.NextCursor == nil {
		t.Fatalf("EnvironmentPage item count = %d, want 100 with a next cursor", len(page.Items))
	}
}

func TestListEnvironmentEventsHTTPPagesWithoutOverlapOrGapsAndIsStable(t *testing.T) {
	base := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	events := make([]db.EnvironmentEvent, 0, 5)
	for index := 0; index < 5; index++ {
		id := fmt.Sprintf("operation-%d", index+1)
		events = append(events, db.EnvironmentEvent{
			ID: id, EnvironmentID: "environment-1", OperationID: &id,
			Type: "environment.create", Summary: "environment.create queued", CreatedAt: base.Add(time.Duration(index) * time.Minute),
		})
	}
	reads := &environmentReaderFake{expectedOwnerID: "user-1", events: map[string][]db.EnvironmentEvent{"environment-1": events}}
	handler := environmentReadHandler(reads, repeatValue("request", 10))

	first := serveEnvironmentReadRequest(handler, "/v1/environments/environment-1/events?pageSize=2")
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, body:%s", first.Code, first.Body.String())
	}
	var firstPage contracts.EnvironmentEventPage
	if err := json.NewDecoder(first.Body).Decode(&firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 2 || firstPage.NextCursor == nil {
		t.Fatalf("first EnvironmentEventPage = %#v", firstPage)
	}

	second := serveEnvironmentReadRequest(handler, "/v1/environments/environment-1/events?pageSize=2&cursor="+url.QueryEscape(*firstPage.NextCursor))
	var secondPage contracts.EnvironmentEventPage
	if err := json.NewDecoder(second.Body).Decode(&secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 2 || secondPage.NextCursor == nil {
		t.Fatalf("second EnvironmentEventPage = %#v", secondPage)
	}

	third := serveEnvironmentReadRequest(handler, "/v1/environments/environment-1/events?pageSize=2&cursor="+url.QueryEscape(*secondPage.NextCursor))
	var thirdPage contracts.EnvironmentEventPage
	if err := json.NewDecoder(third.Body).Decode(&thirdPage); err != nil {
		t.Fatal(err)
	}
	if len(thirdPage.Items) != 1 || thirdPage.NextCursor != nil {
		t.Fatalf("third (final) EnvironmentEventPage = %#v, want 1 item and no next cursor", thirdPage)
	}

	seen := map[string]bool{}
	var order []string
	for _, page := range []contracts.EnvironmentEventPage{firstPage, secondPage, thirdPage} {
		for _, item := range page.Items {
			if seen[item.Id] {
				t.Fatalf("Environment event %q returned on more than one page: overlap across %#v", item.Id, order)
			}
			seen[item.Id] = true
			order = append(order, item.Id)
		}
	}
	want := []string{"operation-1", "operation-2", "operation-3", "operation-4", "operation-5"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("paged Environment event order = %#v, want %#v (no gaps, stable order)", order, want)
	}
}

func TestListEnvironmentEventsHTTPRejectsInvalidCursor(t *testing.T) {
	reads := &environmentReaderFake{expectedOwnerID: "user-1", events: map[string][]db.EnvironmentEvent{"environment-1": nil}}
	handler := environmentReadHandler(reads, []string{"request-invalid-cursor"})

	response := serveEnvironmentReadRequest(handler, "/v1/environments/environment-1/events?cursor=not-a-valid-cursor")
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"INVALID_CURSOR"`)) {
		t.Fatalf("invalid cursor response = status:%d body:%s", response.Code, response.Body.String())
	}
}

func TestGetEnvironmentHTTPReportsOwnedEnvironmentAndHidesForeign(t *testing.T) {
	detail := newTestEnvironmentDetail(t, "environment-1", "user-1")
	activeOperationID := "operation-1"
	detail.ActiveOperationID = &activeOperationID
	reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
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
	reads := &environmentReaderFake{expectedOwnerID: "user-1", events: map[string][]db.EnvironmentEvent{
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
	return newTestEnvironmentDetailAt(t, id, ownerID, time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC))
}

// newTestEnvironmentDetailAt builds an EnvironmentDetail stamped with a
// caller-chosen creation time, so pagination tests can construct a series
// of Environments with the distinct (createdAt, id) ordering the keyset
// queries in libs/db rely on.
func newTestEnvironmentDetailAt(t *testing.T, id, ownerID string, createdAt time.Time) db.EnvironmentDetail {
	t.Helper()
	environment, err := domain.RestoreEnvironment(domain.EnvironmentSnapshot{
		ID: id, OwnerUserID: ownerID, Name: "Workspace", Slug: id,
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
	expectedOwnerID string
	environments    map[string]db.EnvironmentDetail
	list            []db.EnvironmentDetail
	events          map[string][]db.EnvironmentEvent
	err             error
	getCalls        int
}

func (fake *environmentReaderFake) GetOwnedEnvironment(_ context.Context, ownerID, environmentID string) (db.EnvironmentDetail, error) {
	fake.getCalls++
	if err := requireOwner("GetOwnedEnvironment", ownerID, fake.expectedOwnerID); err != nil {
		return db.EnvironmentDetail{}, err
	}
	if fake.err != nil {
		return db.EnvironmentDetail{}, fake.err
	}
	detail, found := fake.environments[environmentID]
	if !found {
		return db.EnvironmentDetail{}, db.ErrReferenceNotOwned
	}
	return detail, nil
}

func (fake *environmentReaderFake) ListOwnedEnvironments(_ context.Context, ownerID string, cursor *db.Cursor, pageSize int) ([]db.EnvironmentDetail, *db.Cursor, error) {
	if err := requireOwner("ListOwnedEnvironments", ownerID, fake.expectedOwnerID); err != nil {
		return nil, nil, err
	}
	if fake.err != nil {
		return nil, nil, fake.err
	}
	items, next := paginateFake(fake.list, func(detail db.EnvironmentDetail) (time.Time, string) {
		snapshot := detail.Environment.Snapshot()
		return snapshot.CreatedAt, snapshot.ID
	}, cursor, pageSize)
	return items, next, nil
}

func (fake *environmentReaderFake) ListOwnedEnvironmentEvents(_ context.Context, ownerID, environmentID string, cursor *db.Cursor, pageSize int) ([]db.EnvironmentEvent, *db.Cursor, error) {
	if err := requireOwner("ListOwnedEnvironmentEvents", ownerID, fake.expectedOwnerID); err != nil {
		return nil, nil, err
	}
	if fake.err != nil {
		return nil, nil, fake.err
	}
	events, found := fake.events[environmentID]
	if !found {
		return nil, nil, db.ErrReferenceNotOwned
	}
	items, next := paginateFake(events, func(event db.EnvironmentEvent) (time.Time, string) {
		return event.CreatedAt, event.ID
	}, cursor, pageSize)
	return items, next, nil
}
