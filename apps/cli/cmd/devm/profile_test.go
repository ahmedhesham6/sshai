package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

const (
	testCapsuleRefA   = "owner/user-1/capsule:stable"
	testCapsuleRefB   = "owner/user-1/capsule@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	profilePeerSecret = "PROFILE_PEER_SECRET_MUST_NOT_LEAK"
)

func TestAuthoringProfileStoreRoundTripSelectionAndSafety(t *testing.T) {
	directory := filepath.Join(t.TempDir(), ".config", "devm")
	store := newLocalStateStore(directory)
	record := authoringProfileFromContracts("profile_01", "Personal", "version_01", []contracts.CapsuleRef{{
		Ref: testCapsuleRefA, FreshnessPolicy: contracts.Review, Exclusions: &[]string{"config:editor"},
	}})
	if err := store.SaveAuthoringProfile(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.ReadAuthoringProfile("profile_01")
	if err != nil || !found || loaded.ProfileID != record.ProfileID || loaded.LastObservedHeadVersionID != "version_01" ||
		len(loaded.CapsuleRefs) != 1 || loaded.CapsuleRefs[0].FreshnessPolicy != "review" || len(loaded.CapsuleRefs[0].Exclusions) != 1 {
		t.Fatalf("round trip = %#v found:%t error:%v", loaded, found, err)
	}
	selected, err := store.ReadSelectedAuthoringProfile()
	if err != nil || selected.ProfileID != "profile_01" {
		t.Fatalf("selected = %#v error:%v", selected, err)
	}
	assertMode(t, filepath.Join(directory, "profiles"), 0o700)
	assertMode(t, filepath.Join(directory, "profiles", "profile_01.toml"), 0o600)
	assertMode(t, filepath.Join(directory, "profiles", "selection.toml"), 0o600)

	if err := os.Chmod(filepath.Join(directory, "profiles", "profile_01.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadAuthoringProfile("profile_01"); err == nil {
		t.Fatal("unsafe authoring Profile permissions were accepted")
	}
	if _, _, err := store.ReadAuthoringProfile("../escape"); err == nil {
		t.Fatal("unsafe Profile ID was accepted")
	}
}

func TestProfileListAndShowPaginateOwnerScopedAndRenderJSON(t *testing.T) {
	var accessToken string
	var pageCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/profiles":
			pageCalls.Add(1)
			if request.URL.Query().Get("pageSize") != "100" {
				t.Errorf("pageSize = %q", request.URL.Query().Get("pageSize"))
			}
			if request.URL.Query().Get("cursor") == "" {
				next := "next-page"
				_ = json.NewEncoder(response).Encode(contracts.ProfilePage{Items: []contracts.ProfileSummary{{Id: "profile_02", Name: "Work", Slug: "work"}}, NextCursor: &next})
				return
			}
			head := "version_01"
			_ = json.NewEncoder(response).Encode(contracts.ProfilePage{Items: []contracts.ProfileSummary{{Id: "profile_01", Name: "Personal", Slug: "personal", HeadVersionId: &head}}})
		case "/v1/profiles/profile_01":
			head := "version_01"
			_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_01", Name: "Personal", Slug: "personal", HeadVersionId: &head})
		case "/v1/profile-versions/version_01":
			_ = json.NewEncoder(response).Encode(profileVersion("version_01", "profile_01", 1, testCapsuleRefA))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	var output bytes.Buffer
	fixture.application.output = &output
	if err := fixture.application.run(context.Background(), []string{"profile", "list", "--json"}); err != nil {
		t.Fatal(err)
	}
	if pageCalls.Load() != 2 || !strings.Contains(output.String(), `"name":"Personal"`) || !strings.Contains(output.String(), `"name":"Work"`) {
		t.Fatalf("list calls:%d output:%q", pageCalls.Load(), output.String())
	}
	output.Reset()
	if err := fixture.application.runProfile(context.Background(), []string{"show", "Personal"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Profile\tPersonal", "version_01", testCapsuleRefA, "track"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("show output %q does not contain %q", output.String(), want)
		}
	}
	assertProfileOutputSafe(t, output.String(), fixture.accessToken)
}

func TestProfileCreateAddRemovePublishAndFork(t *testing.T) {
	var accessToken string
	var createCalls atomic.Int32
	var publishBody contracts.PublishProfileVersionJSONRequestBody
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/profiles":
			if request.Method != http.MethodPost {
				t.Errorf("profiles method = %s", request.Method)
			}
			var body contracts.CreateProfileJSONRequestBody
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			call := createCalls.Add(1)
			if call == 1 {
				if body.Name != "Personal" || body.ForkedFromVersionId != nil {
					t.Errorf("create body = %#v", body)
				}
				response.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_01", Name: "Personal", Slug: "personal"})
				return
			}
			if body.Name != "Forked" || body.ForkedFromVersionId == nil || *body.ForkedFromVersionId != "version_source" {
				t.Errorf("fork body = %#v", body)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_02", Name: "Forked", Slug: "forked"})
		case "/v1/profiles/profile_01/versions":
			if err := json.NewDecoder(request.Body).Decode(&publishBody); err != nil {
				t.Error(err)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(profileVersion("version_new", "profile_01", 1, testCapsuleRefB))
		case "/v1/profile-versions/version_source":
			_ = json.NewEncoder(response).Encode(profileVersion("version_source", "profile_source", 7, testCapsuleRefA))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	var output bytes.Buffer
	fixture.application.output = &output
	ctx := context.Background()
	if err := fixture.application.runProfile(ctx, []string{"create", "Personal"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.application.runProfile(ctx, []string{"add", testCapsuleRefA, "--freshness", "review", "--exclude", "config:editor"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.application.runProfile(ctx, []string{"remove", testCapsuleRefA}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.application.runProfile(ctx, []string{"add", testCapsuleRefB, "--freshness", "pin"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.application.runProfile(ctx, []string{"publish"}); err != nil {
		t.Fatal(err)
	}
	if publishBody.ExpectedHeadVersionId != nil || len(publishBody.CapsuleRefs) != 1 || publishBody.CapsuleRefs[0].Ref != testCapsuleRefB || publishBody.CapsuleRefs[0].FreshnessPolicy != contracts.Pin {
		t.Fatalf("publish body = %#v", publishBody)
	}
	created, found, err := newLocalStateStore(fixture.configDirectory).ReadAuthoringProfile("profile_01")
	if err != nil || !found || created.LastObservedHeadVersionID != "version_new" {
		t.Fatalf("published local state = %#v found:%t err:%v", created, found, err)
	}
	if err := fixture.application.runProfile(ctx, []string{"fork", "version_source", "Forked"}); err != nil {
		t.Fatal(err)
	}
	forked, err := newLocalStateStore(fixture.configDirectory).ReadSelectedAuthoringProfile()
	if err != nil || forked.ProfileID != "profile_02" || len(forked.CapsuleRefs) != 1 || forked.CapsuleRefs[0].Ref != testCapsuleRefA {
		t.Fatalf("forked local state = %#v err:%v", forked, err)
	}
	assertProfileOutputSafe(t, output.String(), fixture.accessToken)
}

func TestProfilePublishStaleHeadExplainsChangesRefreshAndFork(t *testing.T) {
	var accessToken string
	head := "version_current"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/profiles/profile_01/versions":
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":{"code":"STALE_PROFILE_HEAD","message":"` + profilePeerSecret + `"},"requestId":"request-1"}`))
		case "/v1/profiles":
			_ = json.NewEncoder(response).Encode(contracts.ProfilePage{Items: []contracts.ProfileSummary{{Id: "profile_01", Name: "Personal", Slug: "personal", HeadVersionId: &head}}})
		case "/v1/profiles/profile_01":
			_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_01", Name: "Personal", Slug: "personal", HeadVersionId: &head})
		case "/v1/profile-versions/version_current":
			_ = json.NewEncoder(response).Encode(profileVersion("version_current", "profile_01", 3, testCapsuleRefB))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	record := authoringProfileFromContracts("profile_01", "Personal", "version_old", []contracts.CapsuleRef{{Ref: testCapsuleRefA, FreshnessPolicy: contracts.Track}})
	if err := newLocalStateStore(fixture.configDirectory).SaveAuthoringProfile(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	err := fixture.application.runProfile(context.Background(), []string{"publish"})
	if err == nil {
		t.Fatal("stale publish succeeded")
	}
	message := err.Error()
	for _, want := range []string{"stale head", "version_old", "version_current", "intervening Capsule Ref changes", "Refresh", "devm profile fork version_current", "automatic merging is not supported"} {
		if !strings.Contains(message, want) {
			t.Fatalf("stale UX %q does not contain %q", message, want)
		}
	}
	assertProfileOutputSafe(t, message, fixture.accessToken)
}

func TestProfileApplyResolvesBindingPollsAndSupportsNoWait(t *testing.T) {
	for _, noWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll", true: "no-wait"}[noWait], func(t *testing.T) {
			var accessToken string
			var polls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				assertProfileAuthorization(t, request, accessToken)
				response.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/v1/environments/env_01/apply-profile":
					var body contracts.ApplyEnvironmentProfileJSONRequestBody
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.ProfileVersionId != "version_apply" {
						t.Errorf("apply body = %#v err:%v", body, err)
					}
					response.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(response).Encode(contracts.EnvironmentOperation{Environment: lifecycleEnvironment("env_01", "repo", contracts.RuntimeStatusReady), Operation: lifecycleOperation("op_apply", contracts.OperationStatusRunning)})
				case "/v1/operations/op_apply":
					polls.Add(1)
					_ = json.NewEncoder(response).Encode(lifecycleOperation("op_apply", contracts.OperationStatusSucceeded))
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			fixture := newLifecycleFixture(t, server)
			accessToken = fixture.accessToken
			repository := filepath.Join(t.TempDir(), "repo")
			if err := os.Mkdir(repository, 0o700); err != nil {
				t.Fatal(err)
			}
			fixture.application.workingDirectory = func() (string, error) { return repository, nil }
			fixture.application.git = lifecycleGitFake(repository, "git@example.test:owner/repo.git")
			if err := newLocalStateStore(fixture.configDirectory).BindProject(context.Background(), "git://example.test/owner/repo", "env_01"); err != nil {
				t.Fatal(err)
			}
			arguments := []string{"apply", "version_apply"}
			if noWait {
				arguments = append(arguments, "--no-wait")
			}
			var output bytes.Buffer
			fixture.application.output = &output
			if err := fixture.application.runProfile(context.Background(), arguments); err != nil {
				t.Fatal(err)
			}
			wantPolls := int32(1)
			if noWait {
				wantPolls = 0
			}
			if polls.Load() != wantPolls {
				t.Fatalf("polls = %d, want %d", polls.Load(), wantPolls)
			}
			assertProfileOutputSafe(t, output.String(), fixture.accessToken)
		})
	}
}

func TestProfileRefGrammarRejectedBeforeLocalMutation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "devm")
	application := cli{configDirectory: func() (string, error) { return directory, nil }}
	for _, command := range [][]string{{"add", "ghcr.io/user/capsule:latest"}, {"remove", " owner/user-1/capsule:tag"}} {
		err := application.runProfile(context.Background(), command)
		if err == nil || !strings.Contains(err.Error(), "canonical owner-scoped") && !strings.Contains(err.Error(), "surrounding whitespace") {
			t.Fatalf("command %v error = %v", command, err)
		}
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("invalid ref mutated local state: %v", err)
	}
}

func profileVersion(id, profileID string, version int64, ref string) contracts.ProfileVersion {
	return contracts.ProfileVersion{Id: id, ProfileId: profileID, Version: version, Digest: "sha256:" + strings.Repeat("a", 64), CreatedAt: time.Now(), CapsuleRefs: []contracts.CapsuleRef{{Ref: ref, FreshnessPolicy: contracts.Track}}}
}

func assertProfileAuthorization(t *testing.T, request *http.Request, accessToken string) {
	t.Helper()
	if request.Header.Get("Authorization") != "Bearer "+accessToken {
		t.Errorf("authorization = %q", request.Header.Get("Authorization"))
	}
}

func assertProfileOutputSafe(t *testing.T, output, accessToken string) {
	t.Helper()
	for _, secret := range []string{accessToken, lifecycleRefreshSecret, profilePeerSecret} {
		if secret != "" && strings.Contains(output, secret) {
			t.Fatalf("Profile output leaked %q: %q", secret, output)
		}
	}
}
