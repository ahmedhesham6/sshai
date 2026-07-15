//go:build !race

// Restate SDK v1.0.0's test HTTP/2 server races in its request-body drain path.
// Keep the real-server tracer in normal tests; race-test sshai layers separately.
package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/restatedev/sdk-go/ingress"
)

func TestFoundationTracerPersistsAndCompletesThroughRestate(t *testing.T) {
	ctx := context.Background()
	database, connectionString := testfixtures.OpenPostgres(t, ctx)
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	pool, err := pgxpool.New(ctx, connectionString)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	store := db.NewStore(pool)
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, db.EnsureUserInput{
		ID: "user-1", WorkOSUserID: "workos-user-1", DefaultRegion: "us-east-1", ObservedAt: now,
	}); err != nil {
		t.Fatalf("seed User: %v", err)
	}
	for _, statement := range []string{
		`INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-1', 'user-1', 'Default', 'default')`,
		`INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('profile-version-1', 'profile-1', 1, 'sha256:' || repeat('c', 64))`,
		`INSERT INTO ssh_keys (id, owner_user_id, label, algorithm, fingerprint, public_key) VALUES ('ssh-key-1', 'user-1', 'Laptop', 'ssh-ed25519', 'SHA256:key1', 'ssh-ed25519 AAAA')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed creation prerequisite: %v", err)
		}
	}
	fakeProvider := testfixtures.NewProvider()
	restateEnvironment := testfixtures.StartRestate(t,
		workflows.EnvironmentCreateDefinition(
			fakeProvider, workflows.NewEnvironmentCreationActions(store),
			&idsFake{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1"}},
			func() time.Time { return now.Add(time.Minute) },
		),
	)
	failedService := application.NewCreateEnvironmentService(
		store, failedDispatcher{},
		&idsFake{values: []string{"environment-1", "policy-1", "operation-1"}},
		func() time.Time { return now }, map[string]string{"us-east-1": "us-east-1a"},
	)
	failedHandler := controlplane.NewHandler(controlplane.Config{
		CreateEnvironment: failedService,
		RegisterProjectSeed: application.NewRegisterProjectSeedService(
			store, &successfulUploadVerifier{}, &idsFake{values: []string{"project-seed-1", "project-seed-unused"}}, func() time.Time { return now },
		),
		Verifier: verifierFake{}, Users: store,
		UserIDs:       &idsFake{values: []string{"unused-user", "unused-user", "unused-user"}},
		RequestIDs:    &idsFake{values: []string{"request-seed", "request-seed-replay", "request-failed"}},
		DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
	newSeedRequest := func() *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/v1/project-seeds", bytes.NewReader([]byte(`{
			"repositoryUrl":"https://github.com/example/project.git","baseRevision":"abc123",
			"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"manifestDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}`)))
		request.Header.Set("Authorization", "Bearer valid-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "project-seed-key-1")
		return request
	}
	var registeredSeedID, registeredSeedDigest string
	for attempt := 0; attempt < 2; attempt++ {
		seedResponse := httptest.NewRecorder()
		failedHandler.ServeHTTP(seedResponse, newSeedRequest())
		if seedResponse.Code != http.StatusCreated {
			t.Fatalf("Project Seed registration attempt %d status = %d; body = %s", attempt+1, seedResponse.Code, seedResponse.Body.String())
		}
		var created struct{ ID, Digest string }
		if err := json.NewDecoder(seedResponse.Body).Decode(&created); err != nil {
			t.Fatalf("decode Project Seed registration attempt %d: %v", attempt+1, err)
		}
		if attempt == 0 {
			registeredSeedID, registeredSeedDigest = created.ID, created.Digest
		} else if created.ID != registeredSeedID || created.Digest != registeredSeedDigest {
			t.Fatalf("Project Seed replay = %#v, want id:%q digest:%q", created, registeredSeedID, registeredSeedDigest)
		}
	}
	if registeredSeedID != "project-seed-1" || registeredSeedDigest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("registered Project Seed = id:%q digest:%q", registeredSeedID, registeredSeedDigest)
	}
	var seedRows, gitBundles, trackedPatches, untrackedBundles int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(git_bundle_digest), count(tracked_patch_digest), count(untracked_bundle_digest)
		FROM project_seeds WHERE owner_user_id = 'user-1'`).Scan(&seedRows, &gitBundles, &trackedPatches, &untrackedBundles); err != nil {
		t.Fatalf("read registered Project Seed: %v", err)
	}
	if seedRows != 1 || gitBundles != 0 || trackedPatches != 0 || untrackedBundles != 0 {
		t.Fatalf("Project Seed rows = total:%d git:%d patch:%d untracked:%d", seedRows, gitBundles, trackedPatches, untrackedBundles)
	}
	newRequest := func() *http.Request {
		body := fmt.Sprintf(`{
		"name":"API Workspace","region":"us-east-1","runtimePreset":"standard",
		"profileVersionId":"profile-version-1","projectSeedId":%q,
		"autoStopPolicy":{"mode":"manual","gracePeriodSeconds":0},"sshKeyIds":["ssh-key-1"]
		}`, registeredSeedID)
		request := httptest.NewRequest(http.MethodPost, "/v1/environments", bytes.NewReader([]byte(body)))
		request.Header.Set("Authorization", "Bearer valid-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "request-key-0001")
		return request
	}
	failedResponse := httptest.NewRecorder()
	failedHandler.ServeHTTP(failedResponse, newRequest())
	if failedResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed dispatch status = %d, want 503; body = %s", failedResponse.Code, failedResponse.Body.String())
	}
	dispatcher := application.NewEnvironmentCreateDispatcher(store, workflows.NewClient(restateEnvironment.Ingress()))
	recoveryContext, cancelRecovery := context.WithCancel(t.Context())
	recoveryResult := make(chan error, 1)
	go func() {
		recoveryResult <- application.NewWorkflowRecovery(dispatcher, time.Hour, 100, nil).Run(recoveryContext)
	}()
	workflowContext, cancelWorkflow := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelWorkflow()
	workflowOutput, err := awaitEnvironmentCreate(workflowContext, restateEnvironment.Ingress(), "operation-1")
	if err != nil {
		t.Fatalf("await Restate workflow: %v", err)
	}
	if workflowOutput.DataVolumeProviderID != "fake-volume-environment-1" {
		t.Fatalf("workflow Data Volume provider ID = %q", workflowOutput.DataVolumeProviderID)
	}
	cancelRecovery()
	if err := <-recoveryResult; err != nil {
		t.Fatalf("stop workflow recovery: %v", err)
	}
	retryService := application.NewCreateEnvironmentService(
		store, dispatcher, &idsFake{values: []string{"unused-environment", "unused-policy", "unused-operation"}},
		func() time.Time { return now }, map[string]string{"us-east-1": "us-east-1a"},
	)
	retryHandler := controlplane.NewHandler(controlplane.Config{
		CreateEnvironment: retryService, Verifier: verifierFake{}, Users: store,
		UserIDs: &idsFake{values: []string{"unused-user"}}, RequestIDs: &idsFake{values: []string{"request-retry"}},
		DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
	retryResponse := httptest.NewRecorder()
	retryHandler.ServeHTTP(retryResponse, newRequest())
	if retryResponse.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202; body = %s", retryResponse.Code, retryResponse.Body.String())
	}
	var lifecycle, health, operationStatus, invocationID string
	if err := pool.QueryRow(ctx, `
		SELECT e.lifecycle, e.health, o.status, o.restate_invocation_id
		FROM environments e JOIN operations o ON o.environment_id = e.id
		WHERE e.id = 'environment-1' AND o.id = 'operation-1'`).Scan(&lifecycle, &health, &operationStatus, &invocationID); err != nil {
		t.Fatalf("read completed projection: %v", err)
	}
	if lifecycle != "active" || health != "healthy" || operationStatus != "succeeded" {
		t.Fatalf("completed projection = lifecycle:%s health:%s operation:%s", lifecycle, health, operationStatus)
	}
	if invocationID == "" || invocationID == "operation-1" {
		t.Fatalf("actual Restate invocation ID = %q", invocationID)
	}
	if fakeProvider.DataVolumeCreateCount() != 1 {
		t.Fatalf("provider mutations = %d, want 1", fakeProvider.DataVolumeCreateCount())
	}
	var resources, components int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_resources WHERE environment_id = 'environment-1'`).Scan(&resources); err != nil {
		t.Fatalf("count persisted Provider Resources: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_components WHERE environment_id = 'environment-1'`).Scan(&components); err != nil {
		t.Fatalf("count persisted State Components: %v", err)
	}
	if resources != 1 || components != 4 {
		t.Fatalf("persisted Environment State rows = %d/%d", resources, components)
	}
}

func awaitEnvironmentCreate(ctx context.Context, client *ingress.Client, operationID string) (workflows.EnvironmentCreateOutput, error) {
	handle := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		client, workflows.EnvironmentCreateService, operationID,
	)
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for {
		output, err := handle.Attach(ctx)
		var notFound *ingress.InvocationNotFoundError
		if err == nil || !errors.As(err, &notFound) {
			return output, err
		}
		select {
		case <-ctx.Done():
			return workflows.EnvironmentCreateOutput{}, context.Cause(ctx)
		case <-retry.C:
		}
	}
}

type failedDispatcher struct{}

func (failedDispatcher) DispatchEnvironmentCreate(context.Context, string) error {
	return errors.New("simulated crash before Restate send")
}
