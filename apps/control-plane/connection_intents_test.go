package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestCreateConnectionIntentHTTP(t *testing.T) {
	tests := []struct {
		name             string
		environmentID    string
		runtimeStatus    domain.RuntimeStatus
		proxyURLs        map[string]string
		credits          int64
		activeOperation  *string
		wantStatus       int
		wantCode         string
		wantOperationID  *string
		wantReserveCalls int
		wantDispatches   int
	}{
		{name: "ready Runtime", environmentID: "environment-1", runtimeStatus: domain.RuntimeReady, proxyURLs: testRegionalProxyURLs(), credits: 1, wantStatus: http.StatusCreated},
		{name: "starting Runtime reuses active start", environmentID: "environment-1", runtimeStatus: domain.RuntimeStarting, proxyURLs: testRegionalProxyURLs(), credits: 1, activeOperation: stringPointer("operation-active"), wantStatus: http.StatusCreated, wantOperationID: stringPointer("operation-active")},
		{name: "starting Runtime without active start is invalid state", environmentID: "environment-1", runtimeStatus: domain.RuntimeStarting, proxyURLs: testRegionalProxyURLs(), credits: 1, wantStatus: http.StatusUnprocessableEntity, wantCode: "RUNTIME_COMMAND_INVALID_STATE"},
		{name: "stopped Runtime starts", environmentID: "environment-1", runtimeStatus: domain.RuntimeStopped, proxyURLs: testRegionalProxyURLs(), credits: 1, wantStatus: http.StatusCreated, wantOperationID: stringPointer("operation-1"), wantReserveCalls: 1, wantDispatches: 1},
		{name: "credit-blocked start", environmentID: "environment-1", runtimeStatus: domain.RuntimeStopped, proxyURLs: testRegionalProxyURLs(), wantStatus: http.StatusForbidden, wantCode: "CREDITS_POLICY_BLOCKED"},
		{name: "foreign or missing Environment", environmentID: "foreign-environment", runtimeStatus: domain.RuntimeStopped, proxyURLs: testRegionalProxyURLs(), credits: 1, wantStatus: http.StatusNotFound, wantCode: "ENVIRONMENT_NOT_FOUND"},
		{name: "missing regional proxy", environmentID: "environment-1", runtimeStatus: domain.RuntimeStopped, proxyURLs: map[string]string{"eu-west-1": "wss://eu-west.proxy.example.test"}, credits: 1, wantStatus: http.StatusServiceUnavailable, wantCode: "COMMAND_UNAVAILABLE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail := connectionEnvironmentDetail(t, test.runtimeStatus)
			detail.ActiveOperationID = test.activeOperation
			reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
			operations := newConnectionOperationReader()
			if test.activeOperation != nil {
				operations.operations[*test.activeOperation] = db.OperationDetail{Operation: queuedConnectionOperation(*test.activeOperation, "environment-1", domain.OperationRuntimeStart, "active-start-key")}
			}
			repository := &runtimeCommandHTTPRepositoryFake{expectedOwnerID: "user-1", environment: detail.Environment, runtime: *detail.Runtime, operations: map[string]domain.Operation{}}
			dispatcher := &runtimeCommandHTTPDispatcherFake{}
			runtimeCommands := application.NewRuntimeCommandService(repository, dispatcher, runtimeCommandBalanceHTTPFake{credits: test.credits}, &idsFake{values: []string{"operation-1"}}, fixedNow)
			intentIDs := &idsFake{values: []string{"intent-1"}}
			handler := connectionIntentHandler(reads, operations, runtimeCommands, test.proxyURLs, intentIDs, 0, 1)

			response := serveConnectionIntentRequest(handler, test.environmentID, "runtime-request-0001")
			if reads.getCalls != 1 {
				t.Fatalf("owned Environment reads = %d, want one loaded detail", reads.getCalls)
			}

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body:%s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantCode != "" && !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+test.wantCode+`"`)) {
				t.Fatalf("response body = %s", response.Body.String())
			}
			if repository.reserveCalls != test.wantReserveCalls || len(dispatcher.operationIDs) != test.wantDispatches {
				t.Fatalf("start calls = reserve:%d dispatch:%#v, want reserve:%d dispatches:%d", repository.reserveCalls, dispatcher.operationIDs, test.wantReserveCalls, test.wantDispatches)
			}
			if test.wantStatus != http.StatusCreated {
				if intentIDs.index != 0 {
					t.Fatalf("minted %d Connection Intent IDs for refused request", intentIDs.index)
				}
				return
			}
			if intentIDs.index != 1 {
				t.Fatalf("minted %d Connection Intent IDs, want 1", intentIDs.index)
			}
			var intent contracts.ConnectionIntent
			if err := json.NewDecoder(response.Body).Decode(&intent); err != nil {
				t.Fatal(err)
			}
			if intent.Id != "intent-1" || intent.EnvironmentId != "environment-1" || intent.LogicalHostname != "environment-1" || !equalOptionalString(intent.OperationId, test.wantOperationID) {
				t.Fatalf("Connection Intent = %#v", intent)
			}
			parsed, err := url.Parse(intent.ProxyUrl)
			if err != nil || parsed.Scheme != "wss" || parsed.Host != "us-east.proxy.example.test" || parsed.Path != "/v1/environments/environment-1/ssh" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
				t.Fatalf("proxyUrl = %q (%#v, %v)", intent.ProxyUrl, parsed, err)
			}
			if !intent.ExpiresAt.Equal(fixedNow().Add(time.Minute)) {
				t.Fatalf("expiresAt = %s, want %s", intent.ExpiresAt, fixedNow().Add(time.Minute))
			}
		})
	}
}

func TestCreateConnectionIntentReplaysStartOperation(t *testing.T) {
	detail := connectionEnvironmentDetail(t, domain.RuntimeStopped)
	reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
	operations := newConnectionOperationReader()
	repository := &runtimeCommandHTTPRepositoryFake{expectedOwnerID: "user-1", environment: detail.Environment, runtime: *detail.Runtime, operations: map[string]domain.Operation{}, reads: reads, operationReads: operations}
	dispatcher := &runtimeCommandHTTPDispatcherFake{}
	runtimeCommands := application.NewRuntimeCommandService(repository, dispatcher, runtimeCommandBalanceHTTPFake{credits: 1}, &idsFake{values: []string{"operation-1", "operation-unused"}}, fixedNow)
	intentIDs := &idsFake{values: []string{"intent-1", "intent-unused"}}
	handler := connectionIntentHandler(reads, operations, runtimeCommands, testRegionalProxyURLs(), intentIDs, time.Minute, 2)

	first := serveConnectionIntentRequest(handler, "environment-1", "runtime-request-0001")
	second := serveConnectionIntentRequest(handler, "environment-1", "runtime-request-0001")

	for index, response := range []*httptest.ResponseRecorder{first, second} {
		if response.Code != http.StatusCreated {
			t.Fatalf("response %d status = %d; body:%s", index, response.Code, response.Body.String())
		}
		var intent contracts.ConnectionIntent
		if err := json.NewDecoder(response.Body).Decode(&intent); err != nil {
			t.Fatal(err)
		}
		if intent.OperationId == nil || *intent.OperationId != "operation-1" {
			t.Fatalf("response %d Operation ID = %v", index, intent.OperationId)
		}
	}
	if repository.reserveCalls != 1 || repository.replayCalls != 1 {
		t.Fatalf("Operation persistence calls = reserve:%d replay:%d", repository.reserveCalls, repository.replayCalls)
	}
	for _, operationID := range dispatcher.operationIDs {
		if operationID != "operation-1" {
			t.Fatalf("dispatched duplicate Operation: %#v", dispatcher.operationIDs)
		}
	}
	if intentIDs.index != 1 {
		t.Fatalf("minted Connection Intent identities = %d, want 1", intentIDs.index)
	}
}

func TestCreateConnectionIntentGateScenarios(t *testing.T) {
	t.Run("replay after Runtime becomes ready returns stored start Operation", func(t *testing.T) {
		detail := connectionEnvironmentDetail(t, domain.RuntimeStopped)
		reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
		operations := newConnectionOperationReader()
		runtimeRepository := &runtimeCommandHTTPRepositoryFake{expectedOwnerID: "user-1", environment: detail.Environment, runtime: *detail.Runtime, operations: map[string]domain.Operation{}, reads: reads, operationReads: operations}
		runtimeCommands := application.NewRuntimeCommandService(runtimeRepository, &runtimeCommandHTTPDispatcherFake{}, runtimeCommandBalanceHTTPFake{credits: 1}, &idsFake{values: []string{"operation-1"}}, fixedNow)
		intentIDs := &idsFake{values: []string{"intent-1", "intent-unused"}}
		handler := connectionIntentHandler(reads, operations, runtimeCommands, testRegionalProxyURLs(), intentIDs, time.Minute, 2)

		first := serveConnectionIntentRequest(handler, "environment-1", "connection-key-0001")
		ready := connectionEnvironmentDetail(t, domain.RuntimeReady)
		reads.environments["environment-1"] = ready
		second := serveConnectionIntentRequest(handler, "environment-1", "connection-key-0001")
		firstIntent, secondIntent := decodeConnectionIntent(t, first), decodeConnectionIntent(t, second)
		if first.Code != http.StatusCreated || second.Code != http.StatusCreated || firstIntent.Id != secondIntent.Id ||
			firstIntent.OperationId == nil || secondIntent.OperationId == nil || *firstIntent.OperationId != "operation-1" || *secondIntent.OperationId != "operation-1" || intentIDs.index != 1 {
			t.Fatalf("replay = first:%d/%#v second:%d/%#v minted:%d", first.Code, firstIntent, second.Code, secondIntent, intentIDs.index)
		}
	})

	t.Run("cross Environment key reuse conflicts", func(t *testing.T) {
		first := connectionEnvironmentDetail(t, domain.RuntimeReady)
		second := first
		secondSnapshot := second.Environment.Snapshot()
		secondSnapshot.ID = "environment-2"
		secondEnvironment, err := domain.RestoreEnvironment(secondSnapshot)
		if err != nil {
			t.Fatal(err)
		}
		second.Environment = secondEnvironment
		reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": first, "environment-2": second}}
		intentIDs := &idsFake{values: []string{"intent-1", "intent-unused"}}
		handler := connectionIntentHandler(reads, newConnectionOperationReader(), nil, testRegionalProxyURLs(), intentIDs, time.Minute, 2)

		created := serveConnectionIntentRequest(handler, "environment-1", "connection-key-0001")
		conflict := serveConnectionIntentRequest(handler, "environment-2", "connection-key-0001")
		if created.Code != http.StatusCreated || conflict.Code != http.StatusConflict || !bytes.Contains(conflict.Body.Bytes(), []byte(`"code":"IDEMPOTENCY_CONFLICT"`)) || intentIDs.index != 1 {
			t.Fatalf("cross-Environment reuse = created:%d conflict:%d/%s minted:%d", created.Code, conflict.Code, conflict.Body.String(), intentIDs.index)
		}
	})

	t.Run("active stop refuses without minting", func(t *testing.T) {
		detail := connectionEnvironmentDetail(t, domain.RuntimeReady)
		detail.ActiveOperationID = stringPointer("operation-stop")
		reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
		operations := newConnectionOperationReader()
		operations.operations["operation-stop"] = db.OperationDetail{Operation: queuedConnectionOperation("operation-stop", "environment-1", domain.OperationRuntimeStop, "stop-key-0000001")}
		intentIDs := &idsFake{values: []string{"intent-unused"}}
		handler := connectionIntentHandler(reads, operations, nil, testRegionalProxyURLs(), intentIDs, time.Minute, 1)

		response := serveConnectionIntentRequest(handler, "environment-1", "connection-key-0001")
		if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"OPERATION_CONFLICT"`)) || intentIDs.index != 0 {
			t.Fatalf("stop race = %d/%s minted:%d", response.Code, response.Body.String(), intentIDs.index)
		}
	})

	t.Run("different keys join one start while stopped projection lags", func(t *testing.T) {
		detail := connectionEnvironmentDetail(t, domain.RuntimeStopped)
		reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
		operations := newConnectionOperationReader()
		runtimeRepository := &runtimeCommandHTTPRepositoryFake{expectedOwnerID: "user-1", environment: detail.Environment, runtime: *detail.Runtime, operations: map[string]domain.Operation{}, reads: reads, operationReads: operations, publishActiveOnConflict: true}
		runtimeCommands := application.NewRuntimeCommandService(runtimeRepository, &runtimeCommandHTTPDispatcherFake{}, runtimeCommandBalanceHTTPFake{credits: 1}, &idsFake{values: []string{"operation-1", "operation-unused"}}, fixedNow)
		handler := connectionIntentHandler(reads, operations, runtimeCommands, testRegionalProxyURLs(), &idsFake{values: []string{"intent-1", "intent-2"}}, time.Minute, 2)

		first := decodeConnectionIntent(t, serveConnectionIntentRequest(handler, "environment-1", "connection-key-0001"))
		secondResponse := serveConnectionIntentRequest(handler, "environment-1", "connection-key-0002")
		second := decodeConnectionIntent(t, secondResponse)
		if secondResponse.Code != http.StatusCreated || first.OperationId == nil || second.OperationId == nil || *first.OperationId != "operation-1" || *second.OperationId != "operation-1" || runtimeRepository.reserveCalls != 2 {
			t.Fatalf("joined starts = first:%#v second:%d/%#v reserves:%d", first, secondResponse.Code, second, runtimeRepository.reserveCalls)
		}
	})
}

func TestRegionalProxyURLsAreValidatedAtHandlerConstruction(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
	}{
		{name: "HTTPS", url: "https://proxy.example.test"},
		{name: "credentials", url: "wss://user@proxy.example.test"},
		{name: "query", url: "wss://proxy.example.test?token=secret"},
		{name: "fragment", url: "wss://proxy.example.test#fragment"},
		{name: "missing host", url: "wss:///proxy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewHandler() did not reject invalid regional proxy URL")
				}
			}()
			_ = controlplane.NewHandler(controlplane.Config{RegionalProxyURLs: map[string]string{"us-east-1": test.url}})
		})
	}
}

func connectionIntentHandler(reads controlplane.EnvironmentReader, operationReads controlplane.OperationReader, runtimeCommands *application.RuntimeCommandService, proxyURLs map[string]string, intentIDs application.IDGenerator, ttl time.Duration, requests int) http.Handler {
	userIDs := make([]string, requests)
	requestIDs := make([]string, requests)
	for index := range requests {
		userIDs[index] = "user-1"
		requestIDs[index] = "request-1"
	}
	return controlplane.NewHandler(controlplane.Config{
		RuntimeCommands: runtimeCommands, EnvironmentReads: reads, OperationReads: operationReads,
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: userIDs},
		RequestIDs: &idsFake{values: requestIDs}, ConnectionIntentIDs: intentIDs, ConnectionIntents: newConnectionIntentRepositoryFake(),
		DefaultRegion: "us-east-1", Now: fixedNow, RegionalProxyURLs: proxyURLs, ConnectionIntentTTL: ttl,
	})
}

type connectionIntentRepositoryFake struct {
	records map[string]db.ConnectionIntentRecord
}

func newConnectionIntentRepositoryFake() *connectionIntentRepositoryFake {
	return &connectionIntentRepositoryFake{records: map[string]db.ConnectionIntentRecord{}}
}

func (fake *connectionIntentRepositoryFake) CreateOrReplayConnectionIntent(ctx context.Context, ownerID, key, environmentID string, observedAt, expiresAt time.Time, prepare func(context.Context) (*string, error), mint func() string) (db.ConnectionIntentRecord, error) {
	mapKey := ownerID + "\x00" + key
	if existing, present := fake.records[mapKey]; present && existing.ExpiresAt.After(observedAt) {
		if existing.EnvironmentID != environmentID {
			return db.ConnectionIntentRecord{}, db.ErrIdempotencyConflict
		}
		return existing, nil
	}
	operationID, err := prepare(ctx)
	if err != nil {
		return db.ConnectionIntentRecord{}, err
	}
	record := db.ConnectionIntentRecord{OwnerUserID: ownerID, IdempotencyKey: key, EnvironmentID: environmentID, OperationID: operationID, IntentID: mint(), ExpiresAt: expiresAt}
	fake.records[mapKey] = record
	return record, nil
}

func newConnectionOperationReader() *operationReaderFake {
	return &operationReaderFake{expectedOwnerID: "user-1", operations: map[string]db.OperationDetail{}}
}

func queuedConnectionOperation(id, environmentID string, operationType domain.OperationType, key string) domain.Operation {
	operation, err := domain.QueueOperation(domain.OperationRequest{ID: id, EnvironmentID: environmentID, Type: operationType, RequestedByUserID: "user-1", IdempotencyKey: key, Input: []byte(`{}`), CreatedAt: fixedNow()})
	if err != nil {
		panic(err)
	}
	return operation
}

func decodeConnectionIntent(t *testing.T, response *httptest.ResponseRecorder) contracts.ConnectionIntent {
	t.Helper()
	var intent contracts.ConnectionIntent
	if response.Code == http.StatusCreated {
		if err := json.NewDecoder(response.Body).Decode(&intent); err != nil {
			t.Fatal(err)
		}
	}
	return intent
}

func serveConnectionIntentRequest(handler http.Handler, environmentID, idempotencyKey string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/environments/"+environmentID+"/connection-intents", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func connectionEnvironmentDetail(t *testing.T, status domain.RuntimeStatus) db.EnvironmentDetail {
	t.Helper()
	detail := runtimeEnvironmentDetail(t)
	snapshot := detail.Runtime.Snapshot()
	snapshot.Status = status
	snapshot.StoppedAt = nil
	snapshot.PrivateAddress = nil
	snapshot.BootID = nil
	if status == domain.RuntimeStopped {
		stoppedAt := snapshot.UpdatedAt
		snapshot.StoppedAt = &stoppedAt
	}
	if status == domain.RuntimeReady {
		privateAddress, bootID := "10.0.0.4", "boot-current"
		snapshot.PrivateAddress, snapshot.BootID = &privateAddress, &bootID
	}
	runtime, err := domain.RestoreRuntime(snapshot)
	if err != nil {
		t.Fatalf("RestoreRuntime(): %v", err)
	}
	detail.Runtime = &runtime
	return detail
}

func testRegionalProxyURLs() map[string]string {
	return map[string]string{"us-east-1": "wss://us-east.proxy.example.test/configured/base"}
}

func stringPointer(value string) *string { return &value }

func equalOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
