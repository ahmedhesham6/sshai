package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

const lifecycleRefreshSecret = "LIFECYCLE_REFRESH_SECRET"

type lifecycleFixture struct {
	application     cli
	configDirectory string
	sshDirectory    string
	accessToken     string
}

func newLifecycleFixture(t *testing.T, server *httptest.Server) lifecycleFixture {
	t.Helper()
	root := t.TempDir()
	configDirectory := filepath.Join(root, ".config", "devm")
	sshDirectory := filepath.Join(root, ".ssh")
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	accessToken := testAccessToken(t, now.Add(time.Hour))
	if err := persistCredentials(configDirectory, loginCredentials{accessToken: accessToken, refreshToken: lifecycleRefreshSecret}); err != nil {
		t.Fatal(err)
	}
	application := cli{
		output: io.Discard, errorOutput: io.Discard, input: strings.NewReader(""),
		clientID: "client_public_test", controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
		now:              func() time.Time { return now },
		workingDirectory: func() (string, error) { return root, nil },
		configDirectory:  func() (string, error) { return configDirectory, nil },
		sshDirectory:     func() (string, error) { return sshDirectory, nil },
		newRefreshClient: func(string) (tokenRefresher, error) { return &singleUseRefreshFake{}, nil },
		git:              runGit, runSSHClient: runOpenSSH, wait: waitForContext,
		newAttempt:         func() (string, error) { return "fixture-nonce", nil },
		createPollInterval: time.Millisecond, createWaitTimeout: time.Second,
		stopPollInterval: time.Millisecond, stopWaitTimeout: time.Second,
	}
	return lifecycleFixture{application: application, configDirectory: configDirectory, sshDirectory: sshDirectory, accessToken: accessToken}
}

func TestBareDevmBoundAndUnboundThenHandsOffToGeneratedAlias(t *testing.T) {
	for _, test := range []struct {
		name  string
		bound bool
	}{
		{name: "bound", bound: true},
		{name: "unbound", bound: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var environmentPosts atomic.Int32
			var token string
			var publicKey, fingerprint string
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer "+token {
					t.Errorf("authorization = %q", request.Header.Get("Authorization"))
					http.Error(response, "unauthorized", http.StatusUnauthorized)
					return
				}
				response.Header().Set("Content-Type", "application/json")
				environment := lifecycleEnvironment("env_01", "repo-dev", contracts.RuntimeStatusReady)
				switch request.URL.Path {
				case "/v1/environments/env_01":
					if request.Method != http.MethodGet {
						t.Errorf("bound method = %s", request.Method)
					}
					_ = json.NewEncoder(response).Encode(environment)
				case "/v1/operations/op_create":
					operation := lifecycleOperation("op_create", contracts.OperationStatusSucceeded)
					operation.Type = "environment.create"
					_ = json.NewEncoder(response).Encode(operation)
				case "/v1/environments":
					switch request.Method {
					case http.MethodGet:
						_ = json.NewEncoder(response).Encode(contracts.EnvironmentPage{Items: []contracts.Environment{environment}})
					case http.MethodPost:
						environmentPosts.Add(1)
						if key := request.Header.Get("Idempotency-Key"); !strings.HasPrefix(key, "environment-create-") {
							t.Errorf("create idempotency key = %q", key)
						}
						var body contracts.CreateEnvironmentJSONRequestBody
						if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if body.Region != "eu-central-1" || body.RuntimePreset != "cpu2-mem8" || body.ProfileVersionId != "profile-version-01" ||
							body.ProjectSeedId != "project-seed-01" || len(body.SshKeyIds) != 1 || body.AutoStopPolicy.GracePeriodSeconds != 300 {
							t.Errorf("create body = %#v", body)
						}
						response.WriteHeader(http.StatusAccepted)
						operation := lifecycleOperation("op_create", contracts.OperationStatusQueued)
						operation.Type = "environment.create"
						_ = json.NewEncoder(response).Encode(contracts.EnvironmentOperation{Environment: environment, Operation: operation})
					default:
						http.Error(response, "method", http.StatusMethodNotAllowed)
					}
				case "/v1/ssh-keys":
					_ = json.NewEncoder(response).Encode(contracts.SSHKeyPage{Items: []contracts.SSHKey{{
						Id: "key_01", Label: "id_test", Algorithm: contracts.SshEd25519, PublicKey: publicKey,
						Fingerprint: fingerprint, CreatedAt: time.Now(),
					}}})
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			fixture := newLifecycleFixture(t, server)
			token = fixture.accessToken
			publicKey, fingerprint = writeEd25519KeyPair(t, fixture.sshDirectory, "id_test", "")
			repositoryRoot := filepath.Join(t.TempDir(), "repo")
			if err := os.Mkdir(repositoryRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			identity := "git://git@example.test/owner/repo"
			fixture.application.workingDirectory = func() (string, error) { return repositoryRoot, nil }
			fixture.application.git = lifecycleGitFake(repositoryRoot, "git@example.test:owner/repo.git")
			store := newLocalStateStore(fixture.configDirectory)
			if err := store.SetProjectSeed(context.Background(), identity, "project-seed-01"); err != nil {
				t.Fatal(err)
			}
			if test.bound {
				if err := store.BindProject(context.Background(), identity, "env_01"); err != nil {
					t.Fatal(err)
				}
			} else if err := store.UpdateConfig(context.Background(), func(config *localConfig) error {
				config.ProfileVersionID = "profile-version-01"
				config.SSHKeyIDs = []string{"key_01"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var alias string
			fixture.application.runSSHClient = func(_ context.Context, got string, _ io.Reader, _, _ io.Writer) error {
				alias = got
				return nil
			}
			var output bytes.Buffer
			fixture.application.output = &output
			if err := fixture.application.run(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if alias != "repo-dev" {
				t.Fatalf("SSH alias = %q", alias)
			}
			wantPosts := int32(0)
			if !test.bound {
				wantPosts = 1
			}
			if environmentPosts.Load() != wantPosts {
				t.Fatalf("Environment POSTs = %d, want %d", environmentPosts.Load(), wantPosts)
			}
			binding, found, err := store.ReadProject(identity)
			if err != nil || !found || binding.EnvironmentID != "env_01" {
				t.Fatalf("saved binding = %#v found:%t error:%v", binding, found, err)
			}
			assertLifecycleOutputSafe(t, output.String(), fixture.accessToken)
		})
	}
}

func TestBareDevmUnauthenticatedPointsToLogin(t *testing.T) {
	root := t.TempDir()
	application := cli{
		clientID: "client", controlPlaneURL: "https://control.example/v1", now: time.Now,
		workingDirectory: func() (string, error) { return root, nil },
		configDirectory:  func() (string, error) { return filepath.Join(root, "devm"), nil },
		git:              lifecycleGitFake(root, "git@example.test:owner/repo.git"),
		newRefreshClient: func(string) (tokenRefresher, error) { return &singleUseRefreshFake{}, nil },
	}
	err := application.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "devm login") || strings.Contains(err.Error(), lifecycleRefreshSecret) {
		t.Fatalf("unauthenticated error = %v", err)
	}
}

func TestBareDevmFirstRunSelfProvisionsRequiredReferences(t *testing.T) {
	var token string
	var key *contracts.SSHKey
	var created bool
	var seedRepositoryURL string
	var uploads atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/storage/") {
			if request.Header.Get("Authorization") != "" {
				t.Error("storage upload received the bearer token")
			}
			content, err := io.ReadAll(request.Body)
			if err != nil || request.ContentLength != int64(len(content)) {
				t.Errorf("upload content length = %d/%d error=%v", request.ContentLength, len(content), err)
			}
			uploads.Add(1)
			response.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		environment := lifecycleEnvironment("env_first", "repo-first", contracts.RuntimeStatusReady)
		switch request.URL.Path {
		case "/v1/ssh-keys":
			if request.Method == http.MethodPost {
				var body contracts.CreateSSHKeyJSONRequestBody
				_ = json.NewDecoder(request.Body).Decode(&body)
				key = &contracts.SSHKey{Id: "key_first", Label: body.Label, Algorithm: contracts.SshEd25519, PublicKey: body.PublicKey}
				_, key.Fingerprint, _ = parseEd25519PublicKey([]byte(body.PublicKey))
				response.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(response).Encode(key)
				return
			}
			items := []contracts.SSHKey{}
			if key != nil {
				items = append(items, *key)
			}
			_ = json.NewEncoder(response).Encode(contracts.SSHKeyPage{Items: items})
		case "/v1/profiles":
			if request.Method == http.MethodPost {
				response.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_first", Name: "Default", Slug: "default"})
				return
			}
			_ = json.NewEncoder(response).Encode(contracts.ProfilePage{Items: []contracts.ProfileSummary{}})
		case "/v1/profiles/profile_first/versions":
			var body contracts.PublishProfileVersionJSONRequestBody
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body.CapsuleRefs == nil || len(body.CapsuleRefs) != 0 {
				t.Errorf("minimal Profile Version refs = %#v", body.CapsuleRefs)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(contracts.ProfileVersion{Id: "profile_version_first", ProfileId: "profile_first", CapsuleRefs: []contracts.CapsuleRef{}})
		case "/v1/uploads":
			var body contracts.CreateUploadIntentJSONRequestBody
			_ = json.NewDecoder(request.Body).Decode(&body)
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(map[string]any{
				"uploadId": "upload_" + string(body.Kind), "url": server.URL + "/storage/" + string(body.Kind),
				"expiresAt": time.Now().Add(time.Hour), "requiredHeaders": map[string]string{"Content-Length": fmt.Sprint(body.SizeBytes)},
			})
		case "/v1/project-seeds":
			var body contracts.CreateProjectSeedJSONRequestBody
			_ = json.NewDecoder(request.Body).Decode(&body)
			seedRepositoryURL = body.RepositoryUrl
			if strings.Contains(body.RepositoryUrl, "token") || strings.Contains(body.RepositoryUrl, "secret") {
				t.Errorf("Project Seed repository URL leaked credentials: %q", body.RepositoryUrl)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(map[string]string{"id": "seed_first", "digest": body.Digest})
		case "/v1/environments":
			if request.Method == http.MethodPost {
				var body contracts.CreateEnvironmentJSONRequestBody
				_ = json.NewDecoder(request.Body).Decode(&body)
				if body.ProfileVersionId != "profile_version_first" || body.ProjectSeedId != "seed_first" || len(body.SshKeyIds) != 1 || body.SshKeyIds[0] != "key_first" {
					t.Errorf("first-run create body = %#v", body)
				}
				created = true
				operation := lifecycleOperation("op_first", contracts.OperationStatusQueued)
				operation.EnvironmentId, operation.Type = "env_first", "environment.create"
				response.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(response).Encode(contracts.EnvironmentOperation{Environment: environment, Operation: operation})
				return
			}
			items := []contracts.Environment{}
			if created {
				items = append(items, environment)
			}
			_ = json.NewEncoder(response).Encode(contracts.EnvironmentPage{Items: items})
		case "/v1/operations/op_first":
			operation := lifecycleOperation("op_first", contracts.OperationStatusSucceeded)
			operation.EnvironmentId, operation.Type = "env_first", "environment.create"
			_ = json.NewEncoder(response).Encode(operation)
		case "/v1/environments/env_first":
			_ = json.NewEncoder(response).Encode(environment)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	token = fixture.accessToken
	repository := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"init"}, {"config", "user.email", "test@example.test"}, {"config", "user.name", "Test"}} {
		command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("first run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "README.md"}, {"commit", "-m", "initial"}, {"remote", "add", "origin", "https://token:secret@example.test/owner/repo.git?access_token=secret#credential"}} {
		command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	fixture.application.workingDirectory = func() (string, error) { return repository, nil }
	var alias string
	fixture.application.runSSHClient = func(_ context.Context, got string, _ io.Reader, _, _ io.Writer) error { alias = got; return nil }
	var output, errorOutput bytes.Buffer
	fixture.application.output, fixture.application.errorOutput = &output, &errorOutput
	if err := fixture.application.runBare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if alias != "repo-first" || !created || uploads.Load() != 4 || seedRepositoryURL != "https://example.test/owner/repo.git" {
		t.Fatalf("first run alias=%q created=%t uploads=%d repository=%q", alias, created, uploads.Load(), seedRepositoryURL)
	}
	config, err := newLocalStateStore(fixture.configDirectory).ReadConfig()
	if err != nil || config.ProfileVersionID != "profile_version_first" || len(config.SSHKeyIDs) != 1 {
		t.Fatalf("first-run config = %#v error=%v", config, err)
	}
	identity := "git://example.test/owner/repo"
	binding, bound, err := newLocalStateStore(fixture.configDirectory).ReadProject(identity)
	if err != nil || !bound || binding.ProjectSeedID != "seed_first" || binding.EnvironmentID != "env_first" {
		t.Fatalf("first-run binding = %#v bound=%t error=%v", binding, bound, err)
	}
	assertLifecycleOutputSafe(t, output.String()+errorOutput.String(), fixture.accessToken)
}

func TestBareDevmResumesBoundCreateBeforeSSHWithoutPostingAnotherCreate(t *testing.T) {
	var token, publicKey, fingerprint string
	var environmentGets atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		environment := lifecycleEnvironment("env_01", "repo-dev", contracts.RuntimeStatusReady)
		switch request.URL.Path {
		case "/v1/environments/env_01":
			if environmentGets.Add(1) == 1 {
				active := "op_create"
				environment.Lifecycle = contracts.Creating
				environment.ActiveOperationId = &active
			}
			_ = json.NewEncoder(response).Encode(environment)
		case "/v1/operations/op_create":
			operation := lifecycleOperation("op_create", contracts.OperationStatusSucceeded)
			operation.Type = "environment.create"
			_ = json.NewEncoder(response).Encode(operation)
		case "/v1/environments":
			if request.Method == http.MethodPost {
				t.Errorf("resume raced a second create")
			}
			_ = json.NewEncoder(response).Encode(contracts.EnvironmentPage{Items: []contracts.Environment{environment}})
		case "/v1/ssh-keys":
			_ = json.NewEncoder(response).Encode(contracts.SSHKeyPage{Items: []contracts.SSHKey{{
				Id: "key_01", Label: "id_test", Algorithm: contracts.SshEd25519, PublicKey: publicKey, Fingerprint: fingerprint,
			}}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	token = fixture.accessToken
	publicKey, fingerprint = writeEd25519KeyPair(t, fixture.sshDirectory, "id_test", "")
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := "git://git@example.test/owner/repo"
	fixture.application.workingDirectory = func() (string, error) { return root, nil }
	fixture.application.git = lifecycleGitFake(root, "git@example.test:owner/repo.git")
	store := newLocalStateStore(fixture.configDirectory)
	if err := store.SetProjectSeed(context.Background(), identity, "seed_01"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindProject(context.Background(), identity, "env_01"); err != nil {
		t.Fatal(err)
	}
	var alias string
	fixture.application.runSSHClient = func(_ context.Context, got string, _ io.Reader, _, _ io.Writer) error { alias = got; return nil }
	if err := fixture.application.runBare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if alias != "repo-dev" || environmentGets.Load() < 2 {
		t.Fatalf("resume result alias=%q environment GETs=%d", alias, environmentGets.Load())
	}
}

func TestCreateEnvironmentRetriesTypedNameConflictWithIdentityHash(t *testing.T) {
	var names, keys []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body contracts.CreateEnvironmentJSONRequestBody
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		names = append(names, body.Name)
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		response.Header().Set("Content-Type", "application/json")
		if len(names) == 1 {
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":{"code":"ENVIRONMENT_NAME_CONFLICT","message":"conflict"},"requestId":"request_01"}`))
			return
		}
		operation := lifecycleOperation("op_create", contracts.OperationStatusQueued)
		operation.Type = "environment.create"
		response.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(response).Encode(contracts.EnvironmentOperation{
			Environment: lifecycleEnvironment("env_01", "repo-dev", contracts.RuntimeStatusReady), Operation: operation,
		})
	}))
	defer server.Close()
	api, err := contracts.NewClientWithResponses(server.URL, contracts.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	body := contracts.CreateEnvironmentJSONRequestBody{Name: "repo", AutoStopPolicy: contracts.AutoStopPolicy{Mode: contracts.AutoStopPolicyModeManual}}
	if _, err := createLifecycleEnvironment(context.Background(), lifecycleClient{api: api, token: "safe"}, "git://alice@example.test/owner/repo", body); err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "repo" || names[1] != conflictEnvironmentName("repo", "git://alice@example.test/owner/repo") || keys[0] == keys[1] {
		t.Fatalf("conflict retry names=%#v keys=%#v", names, keys)
	}
}

func TestStatusComposesEnvironmentOperationBillingAndJSON(t *testing.T) {
	active := "op_01"
	environment := lifecycleEnvironment("env_01", "repo-dev", contracts.RuntimeStatusReady)
	environment.ActiveOperationId = &active
	operation := lifecycleOperation(active, contracts.OperationStatusRunning)
	billing := contracts.BillingSummary{CreditBalance: 987, SubscriptionStatus: "active"}
	var token string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("missing authorization")
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/environments/env_01":
			_ = json.NewEncoder(response).Encode(environment)
		case "/v1/operations/op_01":
			_ = json.NewEncoder(response).Encode(operation)
		case "/v1/billing":
			_ = json.NewEncoder(response).Encode(billing)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	token = fixture.accessToken
	for _, test := range []struct {
		name  string
		args  []string
		check func(*testing.T, string)
	}{
		{name: "table", args: []string{"status", "--environment", "env_01"}, check: func(t *testing.T, output string) {
			for _, want := range []string{"Runtime\tready", "Auto-stop\twhen_fully_idle, 300s grace", "environment.stop (running)", "Activity\tnot exposed by API yet", "987 (active)"} {
				if !strings.Contains(output, want) {
					t.Fatalf("status output %q lacks %q", output, want)
				}
			}
		}},
		{name: "json", args: []string{"status", "--environment", "env_01", "--json"}, check: func(t *testing.T, output string) {
			var result statusResult
			if err := json.Unmarshal([]byte(output), &result); err != nil || result.Environment.Id != "env_01" || result.Operation == nil || result.Billing.CreditBalance != 987 || result.Activity != "not exposed by API yet" {
				t.Fatalf("status JSON = %#v error:%v", result, err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			fixture.application.output = &output
			if err := fixture.application.run(context.Background(), test.args); err != nil {
				t.Fatal(err)
			}
			test.check(t, output.String())
			assertLifecycleOutputSafe(t, output.String(), fixture.accessToken)
		})
	}
}

func TestStatusRejectsOperationFromAnotherEnvironment(t *testing.T) {
	active := "op_01"
	environment := lifecycleEnvironment("env_01", "repo", contracts.RuntimeStatusReady)
	environment.ActiveOperationId = &active
	operation := lifecycleOperation(active, contracts.OperationStatusRunning)
	operation.EnvironmentId = "env_other"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/environments/env_01":
			_ = json.NewEncoder(response).Encode(environment)
		case "/v1/operations/op_01":
			_ = json.NewEncoder(response).Encode(operation)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	if err := fixture.application.runStatus(context.Background(), []string{"--environment", "env_01", "--json"}); err == nil || !strings.Contains(err.Error(), "another Environment") {
		t.Fatalf("mismatched status Operation error = %v", err)
	}
}

func TestStopPollsToTerminalAndNoWaitSkipsPolling(t *testing.T) {
	for _, noWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll", true: "no-wait"}[noWait], func(t *testing.T) {
			var token, idempotency string
			var polls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer "+token {
					t.Errorf("missing authorization")
				}
				response.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/v1/environments/env_01":
					_ = json.NewEncoder(response).Encode(lifecycleEnvironment("env_01", "repo-dev", contracts.RuntimeStatusReady))
				case "/v1/environments/env_01/stop":
					if request.Method != http.MethodPost {
						t.Errorf("stop method = %s", request.Method)
					}
					key := request.Header.Get("Idempotency-Key")
					idempotency = key
					response.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(response).Encode(contracts.EnvironmentOperation{Operation: lifecycleOperation("op_stop", contracts.OperationStatusRunning)})
				case "/v1/operations/op_stop":
					polls.Add(1)
					_ = json.NewEncoder(response).Encode(lifecycleOperation("op_stop", contracts.OperationStatusSucceeded))
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			fixture := newLifecycleFixture(t, server)
			token = fixture.accessToken
			fixture.application.wait = func(context.Context, time.Duration) error { return nil }
			var output bytes.Buffer
			fixture.application.output = &output
			arguments := []string{"stop", "--environment", "env_01"}
			if noWait {
				arguments = append(arguments, "--no-wait")
			}
			if err := fixture.application.run(context.Background(), arguments); err != nil {
				t.Fatal(err)
			}
			wantPolls := int32(1)
			if noWait {
				wantPolls = 0
			}
			wantKey := deterministicKey("environment-stop", "env_01\x00runtime_01\x00fixture-nonce")
			if polls.Load() != wantPolls || idempotency != wantKey {
				t.Fatalf("stop polls:%d key:%q", polls.Load(), idempotency)
			}
			assertLifecycleOutputSafe(t, output.String(), fixture.accessToken)
		})
	}
}

func TestStopUsesFreshNoncePerInvocationAndCurrentRuntime(t *testing.T) {
	var keys []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/environments/env_01":
			environment := lifecycleEnvironment("env_01", "repo", contracts.RuntimeStatusReady)
			environment.Runtime.Id = "runtime_current"
			_ = json.NewEncoder(response).Encode(environment)
		case "/v1/environments/env_01/stop":
			keys = append(keys, request.Header.Get("Idempotency-Key"))
			response.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(response).Encode(contracts.EnvironmentOperation{Operation: lifecycleOperation("op_stop", contracts.OperationStatusQueued)})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	var nonce atomic.Int32
	fixture.application.newAttempt = func() (string, error) { return fmt.Sprintf("nonce-%d", nonce.Add(1)), nil }
	for range 2 {
		if err := fixture.application.runStop(context.Background(), []string{"--environment", "env_01", "--no-wait"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 || keys[0] == keys[1] || keys[0] != deterministicKey("environment-stop", "env_01\x00runtime_current\x00nonce-1") {
		t.Fatalf("stop invocation keys = %#v", keys)
	}
}

func TestLogoutIsIdempotentAndNeverPrintsCredentials(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	var output bytes.Buffer
	fixture.application.output = &output
	if err := fixture.application.run(context.Background(), []string{"logout"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.application.run(context.Background(), []string{"logout"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Logged out") || !strings.Contains(output.String(), "No local login session existed") {
		t.Fatalf("logout output = %q", output.String())
	}
	assertLifecycleOutputSafe(t, output.String(), fixture.accessToken)
}

func TestDoctorPassesAllChecksFromLocalAndEnvironmentReadModels(t *testing.T) {
	var token string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("missing authorization")
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/me":
			_ = json.NewEncoder(response).Encode(contracts.User{Id: "user_01", DefaultRegion: "eu-central-1"})
		case "/v1/environments":
			_ = json.NewEncoder(response).Encode(contracts.EnvironmentPage{Items: []contracts.Environment{lifecycleEnvironment("env_01", "repo-dev", contracts.RuntimeStatusReady)}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	token = fixture.accessToken
	writeEd25519KeyPair(t, fixture.sshDirectory, "id_test", "")
	if err := newLocalStateStore(fixture.configDirectory).UpdateConfig(context.Background(), func(*localConfig) error { return nil }); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(fixture.configDirectory, "ssh", "config")
	if err := writeOwnedSSHConfig(fixture.configDirectory, ""); err != nil {
		t.Fatal(err)
	}
	if err := ensureSSHInclude(filepath.Join(fixture.sshDirectory, "config"), owned); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	fixture.application.output = &output
	if err := fixture.application.run(context.Background(), []string{"doctor"}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "pass\t") != 7 {
		t.Fatalf("doctor output = %q", output.String())
	}
	for _, check := range []string{"local-state", "authentication", "ssh-include", "ssh-key", "control-plane", "proxy-observation", "guest-observation"} {
		if !strings.Contains(output.String(), "\t"+check+"\t") {
			t.Fatalf("doctor output lacks %s: %q", check, output.String())
		}
	}
	assertLifecycleOutputSafe(t, output.String(), fixture.accessToken)
}

func TestDoctorChecksWarnAndFailWithActionableClientOwnedText(t *testing.T) {
	t.Run("local warnings", func(t *testing.T) {
		root := t.TempDir()
		local := checkDoctorLocalState(filepath.Join(root, "missing"), nil)
		include := checkDoctorSSHInclude(filepath.Join(root, "devm"), filepath.Join(root, ".ssh"), nil, nil)
		key := checkDoctorSSHKey(filepath.Join(root, ".ssh"), nil)
		if local.level != doctorWarn || include.level != doctorFail || key.level != doctorFail {
			t.Fatalf("warning checks = %#v %#v %#v", local, include, key)
		}
	})
	t.Run("unsafe local failures", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		link := filepath.Join(root, "devm")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if result := checkDoctorLocalState(link, nil); result.level != doctorFail {
			t.Fatalf("unsafe local check = %#v", result)
		}
		if result := checkDoctorSSHInclude(root, link, nil, nil); result.level != doctorFail {
			t.Fatalf("unsafe SSH include check = %#v", result)
		}
		state := filepath.Join(root, "state")
		if err := os.Mkdir(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "lock"), filepath.Join(state, "state.lock")); err != nil {
			t.Fatal(err)
		}
		if result := checkDoctorLocalState(state, nil); result.level != doctorFail {
			t.Fatalf("unsafe state lock check = %#v", result)
		}
	})
	t.Run("read model outcomes", func(t *testing.T) {
		proxy, guest := doctorEnvironmentReadModels([]contracts.Environment{}, true)
		if proxy.level != doctorWarn || guest.level != doctorWarn {
			t.Fatalf("empty read models = %#v %#v", proxy, guest)
		}
		errorEnvironment := lifecycleEnvironment("env_01", "repo", contracts.RuntimeStatusError)
		proxy, guest = doctorEnvironmentReadModels([]contracts.Environment{errorEnvironment}, true)
		if proxy.level != doctorFail || guest.level != doctorFail || !strings.Contains(proxy.detail, "devm status") {
			t.Fatalf("error read models = %#v %#v", proxy, guest)
		}
		proxy, guest = doctorEnvironmentReadModels(nil, false)
		if proxy.level != doctorFail || guest.level != doctorFail {
			t.Fatalf("unavailable read models = %#v %#v", proxy, guest)
		}
		invalidHealth := lifecycleEnvironment("env_02", "repo", contracts.RuntimeStatusReady)
		invalidHealth.Health = ""
		proxy, guest = doctorEnvironmentReadModels([]contracts.Environment{invalidHealth}, true)
		if proxy.level == doctorPass || guest.level == doctorPass {
			t.Fatalf("invalid health produced a pass = %#v %#v", proxy, guest)
		}
	})
	t.Run("authentication and control plane failures", func(t *testing.T) {
		root := t.TempDir()
		application := cli{
			output: &bytes.Buffer{}, clientID: "", controlPlaneURL: "https://control.example/v1",
			configDirectory: func() (string, error) { return filepath.Join(root, "devm"), nil },
			sshDirectory:    func() (string, error) { return filepath.Join(root, ".ssh"), nil },
		}
		err := application.runDoctor(context.Background(), nil)
		if err == nil {
			t.Fatal("doctor succeeded without authentication")
		}
		output := application.output.(*bytes.Buffer).String()
		if !strings.Contains(output, "fail\tauthentication\t") || !strings.Contains(output, "fail\tcontrol-plane\t") {
			t.Fatalf("doctor failures = %q", output)
		}
		assertLifecycleOutputSafe(t, output, "ACCESS_SECRET")
	})
}

func lifecycleGitFake(root, remote string) gitRunner {
	return func(_ context.Context, _ string, arguments ...string) (string, error) {
		switch arguments[0] {
		case "rev-parse":
			return root, nil
		case "remote":
			return remote, nil
		default:
			return "", errors.New("unexpected git call")
		}
	}
}

func lifecycleEnvironment(id, slug string, status contracts.RuntimeStatus) contracts.Environment {
	return contracts.Environment{
		Id: id, Name: slug, Slug: slug, Lifecycle: contracts.Active, Health: contracts.EnvironmentHealthHealthy,
		Region: "eu-central-1", RuntimePreset: "cpu2-mem8", PinnedProfileVersionId: "profile-version-01",
		AutoStopPolicy: contracts.AutoStopPolicy{Mode: contracts.AutoStopPolicyModeWhenFullyIdle, GracePeriodSeconds: 300},
		Runtime:        &contracts.Runtime{Id: "runtime_01", Status: status, RuntimePreset: "cpu2-mem8", Region: "eu-central-1", AvailabilityZone: "eu-central-1a", ImageVersion: "v1"},
		CreatedAt:      time.Now(),
	}
}

func lifecycleOperation(id string, status contracts.OperationStatus) contracts.Operation {
	return contracts.Operation{Id: id, EnvironmentId: "env_01", Type: "environment.stop", Status: status, Steps: []contracts.OperationStep{}, CreatedAt: time.Now()}
}

func assertLifecycleOutputSafe(t *testing.T, output, accessToken string) {
	t.Helper()
	for _, secret := range []string{accessToken, lifecycleRefreshSecret, "PEER_MESSAGE_MUST_NOT_BE_PRINTED"} {
		if secret != "" && strings.Contains(output, secret) {
			t.Fatalf("output leaked secret %q: %q", secret, output)
		}
	}
}
