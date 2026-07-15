package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestCreateEnvironmentHTTPTracerCompletesFakeWorkflow(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &creationRepositoryFake{}
	workflow := &completingWorkflowFake{repository: repository, now: now.Add(time.Second)}
	createService := application.NewCreateEnvironmentService(
		repository,
		workflow,
		&idsFake{values: []string{"environment-1", "policy-1", "operation-1"}},
		func() time.Time { return now },
		map[string]string{"us-east-1": "us-east-1a"},
	)
	handler := controlplane.NewHandler(controlplane.Config{
		CreateEnvironment: createService,
		Verifier:          verifierFake{},
		Users:             &usersFake{},
		UserIDs:           &idsFake{values: []string{"user-1"}},
		RequestIDs:        &idsFake{values: []string{"request-1"}},
		DefaultRegion:     "us-east-1",
		Now:               func() time.Time { return now },
	})
	body := []byte(`{
		"name":"API Workspace",
		"region":"us-east-1",
		"runtimePreset":"standard",
		"profileVersionId":"profile-version-1",
		"projectSeedId":"project-seed-1",
		"autoStopPolicy":{"mode":"when_agents_finish","gracePeriodSeconds":300},
		"sshKeyIds":["ssh-key-1"]
	}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/environments", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "request-key-0001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Request-ID"); got != "request-1" {
		t.Fatalf("X-Request-ID = %q, want request-1", got)
	}
	var accepted contracts.EnvironmentOperation
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if accepted.Environment.Id != "environment-1" || accepted.Operation.Id != "operation-1" {
		t.Fatalf("accepted command = %#v", accepted)
	}
	completedEnvironment := repository.completed.Environment().Snapshot()
	completedOperation := repository.completed.Operation().Snapshot()
	if completedEnvironment.Lifecycle != domain.EnvironmentActive || completedEnvironment.Health != domain.EnvironmentHealthHealthy {
		t.Fatalf("fake workflow Environment = %#v", completedEnvironment)
	}
	if completedOperation.Status != domain.OperationSucceeded {
		t.Fatalf("fake workflow Operation = %#v", completedOperation)
	}
}

func TestCreateEnvironmentRequiresBearerAuthentication(t *testing.T) {
	repository := &creationRepositoryFake{}
	handler := controlplane.NewHandler(controlplane.Config{
		CreateEnvironment: application.NewCreateEnvironmentService(
			repository, &completingWorkflowFake{repository: repository}, &idsFake{}, time.Now,
			map[string]string{"us-east-1": "us-east-1a"},
		),
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{},
		RequestIDs: &idsFake{values: []string{"request-unauthorized"}}, DefaultRegion: "us-east-1", Now: time.Now,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/environments", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "request-key-0001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", response.Code, response.Body.String())
	}
	if repository.reserveCalls != 0 {
		t.Fatalf("unauthenticated request reserved %d Environments", repository.reserveCalls)
	}
	if response.Header().Get("X-Request-ID") != "request-unauthorized" {
		t.Fatal("unauthorized response is missing request ID")
	}
}

func TestCreateProjectSeedRegistersAuthenticatedImmutableMetadata(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &projectSeedHTTPRepositoryFake{}
	handler := controlplane.NewHandler(controlplane.Config{
		RegisterProjectSeed: application.NewRegisterProjectSeedService(
			repository, &successfulUploadVerifier{}, &idsFake{values: []string{"seed-1"}}, func() time.Time { return now },
		),
		Verifier: verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
		RequestIDs: &idsFake{values: []string{"request-seed"}}, DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
	body := []byte(`{
		"repositoryUrl":"https://github.com/example/project.git","baseRevision":"abc123",
		"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"gitBundleDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"trackedPatchDigest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"untrackedBundleDigest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"manifestDigest":"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/project-seeds", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "project-seed-key-1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", response.Code, response.Body.String())
	}
	var created struct{ ID, Digest string }
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode Project Seed response: %v", err)
	}
	if created.ID != "seed-1" || created.Digest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("created Project Seed = %#v", created)
	}
	snapshot := repository.seed.Snapshot()
	if snapshot.OwnerUserID != "user-1" || snapshot.GitBundleDigest == "" || snapshot.TrackedPatchDigest == "" || snapshot.UntrackedBundleDigest == "" {
		t.Fatalf("persisted Project Seed = %#v", snapshot)
	}
	if repository.idempotencyKey != "project-seed-key-1" || response.Header().Get("X-Request-ID") != "request-seed" {
		t.Fatalf("registration metadata = key:%q request:%q", repository.idempotencyKey, response.Header().Get("X-Request-ID"))
	}
}

func TestCreateProjectSeedMapsIdempotencyConflict(t *testing.T) {
	repository := &projectSeedHTTPRepositoryFake{err: db.ErrIdempotencyConflict}
	handler := controlplane.NewHandler(controlplane.Config{
		RegisterProjectSeed: application.NewRegisterProjectSeedService(repository, &successfulUploadVerifier{}, &idsFake{values: []string{"seed-1"}}, time.Now),
		Verifier:            verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
		RequestIDs: &idsFake{values: []string{"request-conflict"}}, DefaultRegion: "us-east-1", Now: time.Now,
	})
	body := []byte(`{"repositoryUrl":"https://github.com/example/project.git","baseRevision":"abc123","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","manifestDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/project-seeds", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "project-seed-key-1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"IDEMPOTENCY_CONFLICT"`)) {
		t.Fatalf("conflict response = status:%d body:%s", response.Code, response.Body.String())
	}
	snapshot := repository.seed.Snapshot()
	if snapshot.GitBundleDigest != "" || snapshot.TrackedPatchDigest != "" || snapshot.UntrackedBundleDigest != "" {
		t.Fatalf("omitted optional digests = %#v", snapshot)
	}
}

func TestCreateProjectSeedMapsSafeCommandErrors(t *testing.T) {
	body := []byte(`{"repositoryUrl":"https://github.com/example/project.git","baseRevision":"abc123","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","manifestDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
	for _, scenario := range []struct {
		name, idempotencyKey string
		repositoryError      error
		wantStatus           int
		wantCode             string
	}{
		{name: "invalid command", idempotencyKey: "                ", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PROJECT_SEED"},
		{name: "unavailable repository", idempotencyKey: "project-seed-key-1", repositoryError: errors.New("postgres password=secret"), wantStatus: http.StatusServiceUnavailable, wantCode: "COMMAND_UNAVAILABLE"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repository := &projectSeedHTTPRepositoryFake{err: scenario.repositoryError}
			handler := controlplane.NewHandler(controlplane.Config{
				RegisterProjectSeed: application.NewRegisterProjectSeedService(repository, &successfulUploadVerifier{}, &idsFake{values: []string{"seed-1"}}, time.Now),
				Verifier:            verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
				RequestIDs: &idsFake{values: []string{"request-error"}}, DefaultRegion: "us-east-1", Now: time.Now,
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/project-seeds", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer valid-token")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", scenario.idempotencyKey)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != scenario.wantStatus || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+scenario.wantCode+`"`)) {
				t.Fatalf("error response = status:%d body:%s", response.Code, response.Body.String())
			}
			if response.Header().Get("X-Request-ID") != "request-error" || bytes.Contains(response.Body.Bytes(), []byte("password=secret")) {
				t.Fatalf("unsafe error response = request:%q body:%s", response.Header().Get("X-Request-ID"), response.Body.String())
			}
		})
	}
}

func TestCreateProjectSeedMapsUploadVerificationErrorsSafely(t *testing.T) {
	body := []byte(`{"repositoryUrl":"https://github.com/example/project.git","baseRevision":"abc123","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","manifestDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
	for _, scenario := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid upload", err: application.ErrUploadNotVerified, wantStatus: http.StatusBadRequest, wantCode: "INVALID_UPLOAD"},
		{name: "missing upload intent", err: db.ErrReferenceNotOwned, wantStatus: http.StatusNotFound, wantCode: "UPLOAD_NOT_FOUND"},
		{name: "unavailable object store", err: errors.New("s3 password=secret"), wantStatus: http.StatusServiceUnavailable, wantCode: "COMMAND_UNAVAILABLE"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repository := &projectSeedHTTPRepositoryFake{}
			handler := controlplane.NewHandler(controlplane.Config{
				RegisterProjectSeed: application.NewRegisterProjectSeedService(repository, &successfulUploadVerifier{err: scenario.err}, &idsFake{values: []string{"seed-1"}}, time.Now),
				Verifier:            verifierFake{}, Users: &usersFake{}, UserIDs: &idsFake{values: []string{"user-1"}},
				RequestIDs: &idsFake{values: []string{"request-upload-error"}}, DefaultRegion: "us-east-1", Now: time.Now,
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/project-seeds", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer valid-token")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "project-seed-key-1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != scenario.wantStatus || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"`+scenario.wantCode+`"`)) || bytes.Contains(response.Body.Bytes(), []byte("password=secret")) {
				t.Fatalf("response = status:%d body:%s", response.Code, response.Body.String())
			}
			if repository.seed.Snapshot().ID != "" {
				t.Fatal("unverified Project Seed reached persistence")
			}
		})
	}
}

type verifierFake struct{}

func (verifierFake) Verify(_ context.Context, token string) (auth.Subject, error) {
	if token != "valid-token" {
		return auth.Subject{}, context.Canceled
	}
	return auth.Subject{WorkOSUserID: "workos-user-1"}, nil
}

type usersFake struct{}

func (*usersFake) EnsureUser(_ context.Context, input db.EnsureUserInput) (domain.User, error) {
	return domain.User{
		ID: input.ID, WorkOSUserID: input.WorkOSUserID, DefaultRegion: input.DefaultRegion,
		CreatedAt: input.ObservedAt, UpdatedAt: input.ObservedAt,
	}, nil
}

type creationRepositoryFake struct {
	saved        domain.EnvironmentCreation
	completed    domain.EnvironmentCreation
	reserveCalls int
}

type projectSeedHTTPRepositoryFake struct {
	seed           domain.ProjectSeed
	idempotencyKey string
	err            error
}

func (repository *projectSeedHTTPRepositoryFake) RegisterProjectSeed(_ context.Context, seed domain.ProjectSeed, idempotencyKey string) (domain.ProjectSeed, error) {
	repository.seed, repository.idempotencyKey = seed, idempotencyKey
	return seed, repository.err
}

func (repository *creationRepositoryFake) ReserveEnvironmentCreation(_ context.Context, creation domain.EnvironmentCreation) (domain.EnvironmentCreation, error) {
	repository.reserveCalls++
	if repository.saved.Environment().Snapshot().ID == "" {
		repository.saved = creation
	}
	return repository.saved, nil
}

type completingWorkflowFake struct {
	repository *creationRepositoryFake
	now        time.Time
}

func (workflow *completingWorkflowFake) DispatchEnvironmentCreate(_ context.Context, operationID string) error {
	creation := workflow.repository.saved
	if creation.Operation().Snapshot().ID != operationID {
		return context.Canceled
	}
	creation, err := creation.RecordRestateInvocation("fake-invocation")
	if err != nil {
		return err
	}
	completed, err := creation.Complete(workflow.now)
	if err != nil {
		return err
	}
	workflow.repository.completed = completed
	return nil
}

type idsFake struct {
	values []string
	index  int
}

func (ids *idsFake) NewID() string {
	value := ids.values[ids.index]
	ids.index++
	return value
}
