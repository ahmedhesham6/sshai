package controlplane_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestApplyEnvironmentProfileHTTP(t *testing.T) {
	for _, test := range []struct {
		name           string
		stopped        bool
		foreignVersion bool
		repositoryErr  error
		replay         bool
		wantStatus     int
		wantCode       string
	}{
		{name: "happy 202", wantStatus: http.StatusAccepted},
		{name: "idempotent replay", replay: true, wantStatus: http.StatusAccepted},
		{name: "foreign Profile Version", foreignVersion: true, wantStatus: http.StatusNotFound, wantCode: "PROFILE_VERSION_NOT_FOUND"},
		{name: "active Operation conflict", repositoryErr: db.ErrOperationConflict, wantStatus: http.StatusConflict, wantCode: "OPERATION_CONFLICT"},
		{name: "stopped Runtime", stopped: true, wantStatus: http.StatusUnprocessableEntity, wantCode: "RUNTIME_COMMAND_INVALID_STATE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := profileApplyEnvironmentDetail(t, !test.stopped)
			repository := &runtimeCommandHTTPRepositoryFake{
				expectedOwnerID: "user-1", environment: detail.Environment, runtime: *detail.Runtime, err: test.repositoryErr,
			}
			if test.replay {
				repository.operations = make(map[string]domain.Operation)
			}
			version := profileApplyVersion(t)
			versions := map[string]domain.ProfileVersion{"profile-version-2": version}
			if test.foreignVersion {
				versions = nil
			}
			reads := &environmentReaderFake{expectedOwnerID: "user-1", environments: map[string]db.EnvironmentDetail{"environment-1": detail}}
			profiles := &profileReaderFake{expectedOwnerID: "user-1", versions: versions}
			dispatcher := &runtimeCommandHTTPDispatcherFake{}
			service := application.NewRuntimeCommandService(repository, dispatcher, runtimeCommandBalanceHTTPFake{credits: 1}, &idsFake{values: []string{"operation-1", "operation-unused"}}, fixedNow)
			handler := controlplane.NewHandler(controlplane.Config{
				RuntimeCommands: service, EnvironmentReads: reads, ProfileReads: profiles,
				Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1", "user-1"}},
				RequestIDs: &idsFake{values: []string{"request-1", "request-2"}}, DefaultRegion: "us-east-1", Now: fixedNow,
			})
			response := serveProfileApplyRequest(handler)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body:%s", response.Code, response.Body.String())
			}
			if test.wantCode != "" && !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+test.wantCode+`"`)) {
				t.Fatalf("response body = %s", response.Body.String())
			}
			if test.wantStatus == http.StatusAccepted {
				var accepted contracts.EnvironmentOperation
				if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
					t.Fatal(err)
				}
				if accepted.Operation.Type != string(domain.OperationProfileApply) || accepted.Operation.Id != "operation-1" {
					t.Fatalf("accepted Operation = %#v", accepted.Operation)
				}
				if test.replay {
					repository.replay = repository.operations["profile-apply-request-1"]
					repository.operation = domain.Operation{}
					replayed := serveProfileApplyRequest(handler)
					if replayed.Code != http.StatusAccepted || repository.reserveCalls != 2 {
						t.Fatalf("replay = status:%d reserve:%d lookup:%d body:%s", replayed.Code, repository.reserveCalls, repository.replayCalls, replayed.Body.String())
					}
				}
			}
		})
	}
}

func serveProfileApplyRequest(handler http.Handler) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/environments/environment-1/apply-profile", bytes.NewBufferString(`{"profileVersionId":"profile-version-2","approvedReviewItems":["config:editor"]}`))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "profile-apply-request-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func profileApplyVersion(t *testing.T) domain.ProfileVersion {
	t.Helper()
	version, err := domain.RestoreProfileVersion(domain.ProfileVersionSnapshot{
		ID: "profile-version-2", ProfileID: "profile-1", Version: 2, ParentVersionID: stringPointer("profile-version-1"),
		Digest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CapsuleRefs: []domain.CapsuleRef{{Ref: "owner/user-1/capsule@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FreshnessPolicy: domain.FreshnessPin}},
		CreatedAt:   fixedNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func profileApplyEnvironmentDetail(t *testing.T, ready bool) db.EnvironmentDetail {
	t.Helper()
	detail := runtimeEnvironmentDetail(t)
	if !ready {
		return detail
	}
	snapshot := detail.Runtime.Snapshot()
	privateAddress, bootID := "10.0.0.8", "boot-1"
	snapshot.Status, snapshot.PrivateAddress, snapshot.BootID, snapshot.StoppedAt = domain.RuntimeReady, &privateAddress, &bootID, nil
	runtime, err := domain.RestoreRuntime(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	detail.Runtime = &runtime
	return detail
}
