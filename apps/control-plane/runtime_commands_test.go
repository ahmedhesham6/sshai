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
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestRuntimeCommandHTTPAcceptsStartAndStop(t *testing.T) {
	for _, test := range []struct {
		name, path, body, wantType, wantInput string
	}{
		{name: "start", path: "/v1/environments/environment-1/start", wantType: "runtime.start", wantInput: `{}`},
		{name: "stop manual", path: "/v1/environments/environment-1/stop", body: `{"reason":"manual"}`, wantType: "runtime.stop", wantInput: `{"reason":"manual"}`},
		{name: "stop without body", path: "/v1/environments/environment-1/stop", wantType: "runtime.stop", wantInput: `{"reason":"manual"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := runtimeEnvironmentDetail(t)
			repository := &runtimeCommandHTTPRepositoryFake{
				expectedOwnerID: "user-1", environment: detail.Environment, runtime: *detail.Runtime,
			}
			dispatcher := &runtimeCommandHTTPDispatcherFake{}
			handler := runtimeCommandHandler(detail, repository, dispatcher, nil)
			response := serveRuntimeCommandRequest(handler, test.path, test.body)

			if response.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
			}
			var accepted contracts.EnvironmentOperation
			if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
				t.Fatal(err)
			}
			if accepted.Environment.Id != "environment-1" || accepted.Operation.Id != "operation-1" ||
				accepted.Operation.Type != test.wantType || accepted.Environment.ActiveOperationId == nil ||
				*accepted.Environment.ActiveOperationId != "operation-1" {
				t.Fatalf("EnvironmentOperation = %#v", accepted)
			}
			operation := repository.operation.Snapshot()
			if operation.RequestedByUserID != "user-1" || string(operation.Input) != test.wantInput {
				t.Fatalf("reserved Operation = %#v", operation)
			}
			if len(dispatcher.operationIDs) != 1 || dispatcher.operationIDs[0] != "operation-1" {
				t.Fatalf("dispatched Operations = %#v", dispatcher.operationIDs)
			}
		})
	}
}

func TestRuntimeCommandHTTPMapsOwnershipAndCommandErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		missing    bool
		repository error
		wantStatus int
		wantCode   string
	}{
		{name: "foreign Environment", missing: true, wantStatus: http.StatusNotFound, wantCode: "ENVIRONMENT_NOT_FOUND"},
		{name: "active Operation", repository: db.ErrOperationConflict, wantStatus: http.StatusConflict, wantCode: "OPERATION_CONFLICT"},
		{name: "idempotency key", repository: db.ErrIdempotencyConflict, wantStatus: http.StatusConflict, wantCode: "IDEMPOTENCY_CONFLICT"},
		{name: "credit policy", repository: application.ErrCreditsPolicyBlocked, wantStatus: http.StatusForbidden, wantCode: "CREDITS_POLICY_BLOCKED"},
		{name: "invalid Runtime state", repository: domain.ErrRuntimeCommandState, wantStatus: http.StatusUnprocessableEntity, wantCode: "RUNTIME_COMMAND_INVALID_STATE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := runtimeEnvironmentDetail(t)
			repository := &runtimeCommandHTTPRepositoryFake{
				expectedOwnerID: "user-1", environment: detail.Environment, runtime: *detail.Runtime, err: test.repository,
			}
			dispatcher := &runtimeCommandHTTPDispatcherFake{}
			reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
			path := "/v1/environments/environment-1/start"
			if test.missing {
				path = "/v1/environments/foreign-environment/start"
			}
			handler := runtimeCommandHandlerWithReads(reads, repository, dispatcher, nil)
			response := serveRuntimeCommandRequest(handler, path, "")

			if response.Code != test.wantStatus || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+test.wantCode+`"`)) {
				t.Fatalf("response = status:%d body:%s", response.Code, response.Body.String())
			}
			if test.missing && repository.calls != 0 {
				t.Fatalf("foreign Environment reserved %d Operations", repository.calls)
			}
		})
	}
}

func TestRuntimeCommandHTTPReplaysProjectCurrentEnvironmentState(t *testing.T) {
	tests := []struct {
		name  string
		serve func(t *testing.T, detail db.EnvironmentDetail) *httptest.ResponseRecorder
		check func(t *testing.T, accepted contracts.EnvironmentOperation)
	}{
		{
			name: "terminal Runtime Operation does not become active",
			serve: func(t *testing.T, detail db.EnvironmentDetail) *httptest.ResponseRecorder {
				repository := &runtimeCommandHTTPRepositoryFake{
					expectedOwnerID: "user-1", environment: detail.Environment, runtime: *detail.Runtime,
					replay: succeededOperation(t, domain.OperationRuntimeStart, "operation-historical", `{}`),
				}
				return serveRuntimeCommandRequest(runtimeCommandHandler(detail, repository, &runtimeCommandHTTPDispatcherFake{}, nil), "/v1/environments/environment-1/start", "")
			},
			check: func(t *testing.T, accepted contracts.EnvironmentOperation) {
				if accepted.Operation.Id != "operation-historical" || accepted.Operation.Status != contracts.OperationStatus(domain.OperationSucceeded) ||
					accepted.Environment.ActiveOperationId == nil || *accepted.Environment.ActiveOperationId != "operation-current" {
					t.Fatalf("Runtime replay = %#v", accepted)
				}
			},
		},
		{
			name: "historical Policy does not replace current projection",
			serve: func(t *testing.T, detail db.EnvironmentDetail) *httptest.ResponseRecorder {
				repository := &autoStopPolicyHTTPRepositoryFake{
					expectedOwnerID: "user-1",
					replay:          succeededSynchronousPolicyOperation(t, "operation-policy-historical"),
				}
				service := application.NewAutoStopPolicyService(repository, &idsFake{values: []string{"operation-unused"}}, fixedNow)
				return serveRuntimeCommandRequest(runtimeCommandHandler(detail, nil, nil, service), "/v1/environments/environment-1/auto-stop-policy", `{"mode":"when_fully_idle","gracePeriodSeconds":300}`)
			},
			check: func(t *testing.T, accepted contracts.EnvironmentOperation) {
				if accepted.Operation.Id != "operation-policy-historical" || accepted.Environment.AutoStopPolicy.Mode != contracts.AutoStopPolicyMode(domain.AutoStopManual) ||
					accepted.Environment.AutoStopPolicy.GracePeriodSeconds != 0 || accepted.Environment.ActiveOperationId == nil || *accepted.Environment.ActiveOperationId != "operation-current" {
					t.Fatalf("Policy replay = %#v", accepted)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail := runtimeEnvironmentDetail(t)
			activeOperationID := "operation-current"
			detail.ActiveOperationID = &activeOperationID
			response := test.serve(t, detail)
			if response.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
			}
			var accepted contracts.EnvironmentOperation
			if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
				t.Fatal(err)
			}
			test.check(t, accepted)
		})
	}
}

func TestUpdateAutoStopPolicyHTTP(t *testing.T) {
	for _, test := range []struct {
		name       string
		path       string
		body       string
		wantStatus int
		wantCode   string
		wantCalls  int
	}{
		{name: "updates owned Policy", path: "/v1/environments/environment-1/auto-stop-policy", body: `{"mode":"when_fully_idle","gracePeriodSeconds":300}`, wantStatus: http.StatusAccepted, wantCalls: 1},
		{name: "rejects invalid Policy", path: "/v1/environments/environment-1/auto-stop-policy", body: `{"mode":"never","gracePeriodSeconds":300}`, wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST"},
		{name: "hides foreign Environment", path: "/v1/environments/foreign-environment/auto-stop-policy", body: `{"mode":"manual","gracePeriodSeconds":0}`, wantStatus: http.StatusNotFound, wantCode: "ENVIRONMENT_NOT_FOUND"},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := runtimeEnvironmentDetail(t)
			reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
			repository := &autoStopPolicyHTTPRepositoryFake{expectedOwnerID: "user-1"}
			autoStopPolicies := application.NewAutoStopPolicyService(repository, &idsFake{values: []string{"operation-policy-1"}}, fixedNow)
			handler := runtimeCommandHandlerWithReads(reads, nil, nil, autoStopPolicies)
			response := serveRuntimeCommandRequest(handler, test.path, test.body)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
			}
			if test.wantCode != "" && !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+test.wantCode+`"`)) {
				t.Fatalf("response body = %s", response.Body.String())
			}
			if repository.calls != test.wantCalls {
				t.Fatalf("Policy repository calls = %d, want %d", repository.calls, test.wantCalls)
			}
			if test.wantStatus == http.StatusAccepted {
				var accepted contracts.EnvironmentOperation
				if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
					t.Fatal(err)
				}
				if accepted.Environment.AutoStopPolicy.Mode != contracts.AutoStopPolicyMode(domain.AutoStopWhenFullyIdle) ||
					accepted.Environment.AutoStopPolicy.GracePeriodSeconds != 300 || accepted.Operation.Type != "environment.update_auto_stop" ||
					accepted.Operation.Status != contracts.OperationStatus(domain.OperationSucceeded) {
					t.Fatalf("EnvironmentOperation = %#v", accepted)
				}
				if repository.policy.Snapshot().EnvironmentID != "environment-1" || repository.operation.Snapshot().RequestedByUserID != "user-1" {
					t.Fatalf("persisted Policy/Operation = %#v / %#v", repository.policy.Snapshot(), repository.operation.Snapshot())
				}
			}
		})
	}
}

type runtimeCommandHTTPRepositoryFake struct {
	expectedOwnerID string
	environment     domain.Environment
	runtime         domain.Runtime
	operation       domain.Operation
	replay          domain.Operation
	err             error
	calls           int
}

func (fake *runtimeCommandHTTPRepositoryFake) ReserveRuntimeOperation(_ context.Context, operation domain.Operation) (domain.EnvironmentRuntimeOperation, error) {
	fake.calls++
	if err := requireOwner("ReserveRuntimeOperation", operation.Snapshot().RequestedByUserID, fake.expectedOwnerID); err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	fake.operation = operation
	if fake.replay.Snapshot().ID != "" {
		fake.operation = fake.replay
	}
	if fake.err != nil {
		return domain.EnvironmentRuntimeOperation{}, fake.err
	}
	return domain.NewEnvironmentRuntimeOperation(fake.environment, fake.runtime, fake.operation)
}

func succeededOperation(t *testing.T, operationType domain.OperationType, operationID, input string) domain.Operation {
	t.Helper()
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: operationID, EnvironmentID: "environment-1", Type: operationType,
		RequestedByUserID: "user-1", IdempotencyKey: "runtime-request-0001", Input: []byte(input), CreatedAt: fixedNow().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err = operation.Start(fixedNow().Add(-30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	operation, err = operation.RecordRestateInvocation("invocation-historical")
	if err != nil {
		t.Fatal(err)
	}
	operation, err = operation.Succeed(fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func succeededSynchronousPolicyOperation(t *testing.T, operationID string) domain.Operation {
	t.Helper()
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: operationID, EnvironmentID: "environment-1", Type: domain.OperationEnvironmentUpdateAutoStop,
		RequestedByUserID: "user-1", IdempotencyKey: "runtime-request-0001",
		Input: []byte(`{"gracePeriodSeconds":300,"mode":"when_fully_idle"}`), CreatedAt: fixedNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err = operation.SucceedSynchronously(fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

type runtimeCommandHTTPDispatcherFake struct{ operationIDs []string }

func (fake *runtimeCommandHTTPDispatcherFake) DispatchRuntimeOperation(_ context.Context, operationID string) error {
	fake.operationIDs = append(fake.operationIDs, operationID)
	return nil
}

type autoStopPolicyHTTPRepositoryFake struct {
	expectedOwnerID string
	policy          domain.AutoStopPolicy
	operation       domain.Operation
	replay          domain.Operation
	err             error
	calls           int
}

func (fake *autoStopPolicyHTTPRepositoryFake) UpdateAutoStopPolicy(_ context.Context, ownerID string, policy domain.AutoStopPolicy, operation domain.Operation) (domain.Operation, bool, error) {
	fake.calls++
	if err := requireOwner("UpdateAutoStopPolicy", ownerID, fake.expectedOwnerID); err != nil {
		return domain.Operation{}, false, err
	}
	fake.policy, fake.operation = policy, operation
	if fake.replay.Snapshot().ID != "" {
		return fake.replay, false, fake.err
	}
	return operation, true, fake.err
}

func runtimeCommandHandler(detail db.EnvironmentDetail, repository application.RuntimeOperationRepository, dispatcher application.RuntimeOperationDispatcher, autoStopPolicies *application.AutoStopPolicyService) http.Handler {
	reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
	return runtimeCommandHandlerWithReads(reads, repository, dispatcher, autoStopPolicies)
}

func runtimeCommandHandlerWithReads(reads controlplane.EnvironmentReader, repository application.RuntimeOperationRepository, dispatcher application.RuntimeOperationDispatcher, autoStopPolicies *application.AutoStopPolicyService) http.Handler {
	var runtimeCommands *application.RuntimeCommandService
	if repository != nil {
		runtimeCommands = application.NewRuntimeCommandService(repository, dispatcher, &idsFake{values: []string{"operation-1"}}, fixedNow)
	}
	return controlplane.NewHandler(controlplane.Config{
		RuntimeCommands: runtimeCommands, AutoStopPolicies: autoStopPolicies, EnvironmentReads: reads,
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
		RequestIDs: &idsFake{values: []string{"request-1"}}, DefaultRegion: "us-east-1", Now: fixedNow,
	})
}

func serveRuntimeCommandRequest(handler http.Handler, path, body string) *httptest.ResponseRecorder {
	var requestBody *bytes.Reader
	if body == "" {
		requestBody = bytes.NewReader(nil)
	} else {
		requestBody = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(http.MethodPost, path, requestBody)
	if bytes.Contains([]byte(path), []byte("auto-stop-policy")) {
		request.Method = http.MethodPut
	}
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "runtime-request-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func runtimeEnvironmentDetail(t *testing.T) db.EnvironmentDetail {
	t.Helper()
	createdAt := fixedNow().Add(-time.Hour)
	runtimeID := "runtime-1"
	environment, err := domain.RestoreEnvironment(domain.EnvironmentSnapshot{
		ID: "environment-1", OwnerUserID: "user-1", Name: "Workspace", Slug: "workspace",
		Lifecycle: domain.EnvironmentActive, Health: domain.EnvironmentHealthHealthy,
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-version-1", UpgradePolicy: domain.UpgradeManual,
		CurrentRuntimeID: &runtimeID, AutoStopPolicyID: "policy-1",
		CreatedAt: createdAt, UpdatedAt: createdAt.Add(10 * time.Minute), Version: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	providerID := "i-runtime-1"
	startedAt, stoppedAt := createdAt.Add(time.Minute), createdAt.Add(5*time.Minute)
	runtime, err := domain.RestoreRuntime(domain.RuntimeSnapshot{
		ID: runtimeID, EnvironmentID: "environment-1", Sequence: 1, Status: domain.RuntimeStopped,
		RuntimePreset: "standard", Region: "us-east-1", AvailabilityZone: "us-east-1a", ImageVersion: "image-1",
		ProviderInstanceRef: &providerID, StartedAt: &startedAt, StoppedAt: &stoppedAt,
		CreatedAt: createdAt, UpdatedAt: stoppedAt, Version: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db.EnvironmentDetail{
		Environment: environment, AutoStopMode: domain.AutoStopManual, GracePeriodSeconds: 0, Runtime: &runtime,
	}
}

func fixedNow() time.Time {
	return time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
}
