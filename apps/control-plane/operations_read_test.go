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

func TestGetOperationHTTPReportsOwnedOperationWithSteps(t *testing.T) {
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: "operation-1", EnvironmentID: "environment-1", Type: domain.OperationEnvironmentCreate,
		RequestedByUserID: "user-1", IdempotencyKey: "request-key-0001", Input: []byte(`{}`),
		CreatedAt: time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("queue domain Operation: %v", err)
	}
	reads := &operationReaderFake{operations: map[string]db.OperationDetail{
		"operation-1": {Operation: operation, Steps: []db.OperationStepProjection{{StepKey: "reserve", Status: "running", Summary: "Reserve Environment"}}},
	}}
	handler := operationReadHandler(reads, []string{"request-get"})

	found := serveOperationReadRequest(handler, "/v1/operations/operation-1")
	if found.Code != http.StatusOK {
		t.Fatalf("owned Operation status = %d, body:%s", found.Code, found.Body.String())
	}
	var body contracts.Operation
	if err := json.NewDecoder(found.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Id != "operation-1" || body.EnvironmentId != "environment-1" || body.Status != contracts.OperationStatusQueued || len(body.Steps) != 1 {
		t.Fatalf("Operation = %#v", body)
	}
	if body.Steps[0].StepKey != "reserve" || body.Steps[0].Status != contracts.Running || body.Steps[0].Summary != "Reserve Environment" {
		t.Fatalf("Operation Step = %#v", body.Steps[0])
	}
	if body.CompletedAt != nil {
		t.Fatalf("CompletedAt = %v, want nil for a queued Operation", body.CompletedAt)
	}
}

func TestGetOperationHTTPHidesForeignOperation(t *testing.T) {
	reads := &operationReaderFake{}
	handler := operationReadHandler(reads, []string{"request-foreign"})

	response := serveOperationReadRequest(handler, "/v1/operations/missing-operation")
	if response.Code != http.StatusNotFound || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"OPERATION_NOT_FOUND"`)) {
		t.Fatalf("foreign Operation response = status:%d body:%s", response.Code, response.Body.String())
	}
}

func operationReadHandler(reads controlplane.OperationReader, requestIDs []string) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		OperationReads: reads, Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: repeatValue("user-1", 5)},
		RequestIDs: &idsFake{values: requestIDs}, DefaultRegion: "us-east-1", Now: time.Now,
	})
}

func serveOperationReadRequest(handler http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type operationReaderFake struct {
	operations map[string]db.OperationDetail
	err        error
}

func (fake *operationReaderFake) GetOwnedOperation(_ context.Context, _, operationID string) (db.OperationDetail, error) {
	if fake.err != nil {
		return db.OperationDetail{}, fake.err
	}
	detail, found := fake.operations[operationID]
	if !found {
		return db.OperationDetail{}, db.ErrReferenceNotOwned
	}
	return detail, nil
}
