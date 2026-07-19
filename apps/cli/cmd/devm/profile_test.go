package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

const (
	testCapsuleRefA   = "owner/user-1/capsule:stable"
	testCapsuleRefB   = "owner/user-1/capsule@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testCapsuleRefC   = "owner/user-1/capsule@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
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
		case "/v1/me":
			_ = json.NewEncoder(response).Encode(contracts.User{Id: "user-1", DefaultRegion: "us-east-1"})
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
		case "/v1/me":
			_ = json.NewEncoder(response).Encode(contracts.User{Id: "user-1", DefaultRegion: "us-east-1"})
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
			if body.Name != "Forked" || body.ForkedFromVersionId != nil {
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
		case "/v1/profiles/profile_02/versions":
			var body contracts.PublishProfileVersionJSONRequestBody
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.ExpectedHeadVersionId != nil || len(body.CapsuleRefs) != 1 || body.CapsuleRefs[0].Ref != testCapsuleRefA {
				t.Errorf("fork publication = %#v err:%v", body, err)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(profileVersion("version_fork", "profile_02", 1, testCapsuleRefA))
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
	if err != nil || forked.ProfileID != "profile_02" || forked.LastObservedHeadVersionID != "version_fork" || len(forked.CapsuleRefs) != 1 || forked.CapsuleRefs[0].Ref != testCapsuleRefA {
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
		case "/v1/me":
			_ = json.NewEncoder(response).Encode(contracts.User{Id: "user-1", DefaultRegion: "us-east-1"})
		case "/v1/profiles/profile_01/versions":
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":{"code":"STALE_PROFILE_HEAD","message":"` + profilePeerSecret + `"},"requestId":"request-1"}`))
		case "/v1/profiles":
			_ = json.NewEncoder(response).Encode(contracts.ProfilePage{Items: []contracts.ProfileSummary{{Id: "profile_01", Name: "Personal", Slug: "personal", HeadVersionId: &head}}})
		case "/v1/profiles/profile_01":
			_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_01", Name: "Personal", Slug: "personal", HeadVersionId: &head})
		case "/v1/profile-versions/version_current":
			_ = json.NewEncoder(response).Encode(profileVersion("version_current", "profile_01", 3, testCapsuleRefB))
		case "/v1/profile-versions/version_old":
			_ = json.NewEncoder(response).Encode(profileVersion("version_old", "profile_01", 2, testCapsuleRefA))
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
	for _, want := range []string{"stale head", "version_old", "version_current", "intervening remote Capsule Ref changes", "unpublished local Capsule Ref changes", "devm profile refresh", "devm profile fork --local", "devm profile select profile_01"} {
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

func TestProfileCreateReplayPreservesExistingAuthoringRecord(t *testing.T) {
	var accessToken string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/v1/profiles" {
			http.NotFound(response, request)
			return
		}
		response.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_01", Name: "Personal", Slug: "personal"})
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	ctx := context.Background()
	if err := fixture.application.runProfile(ctx, []string{"create", "Personal"}); err != nil {
		t.Fatal(err)
	}
	store := newLocalStateStore(fixture.configDirectory)
	if err := store.UpdateSelectedAuthoringProfile(ctx, func(profile *localAuthoringProfile) error {
		profile.LastObservedHeadVersionID = "version_existing"
		profile.CapsuleRefs = []localCapsuleRef{{Ref: testCapsuleRefB, FreshnessPolicy: "pin", Exclusions: []string{"config:editor"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadSelectedAuthoringProfile()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.application.runProfile(ctx, []string{"create", "Personal"}); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadSelectedAuthoringProfile()
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("replayed create changed authoring record: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestProfilePublishPreservesConcurrentLocalEditsAndAdvancesCapturedRecord(t *testing.T) {
	var accessToken string
	var store localStateStore
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/me":
			_ = json.NewEncoder(response).Encode(contracts.User{Id: "user-1", DefaultRegion: "us-east-1"})
		case "/v1/profiles/profile_01/versions":
			if err := store.UpdateSelectedAuthoringProfile(context.Background(), func(profile *localAuthoringProfile) error {
				profile.CapsuleRefs = append(profile.CapsuleRefs, localCapsuleRef{Ref: testCapsuleRefC, FreshnessPolicy: "review"})
				return nil
			}); err != nil {
				t.Errorf("concurrent edit: %v", err)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(profileVersion("version_new", "profile_01", 2, testCapsuleRefB))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	store = newLocalStateStore(fixture.configDirectory)
	record := authoringProfileFromContracts("profile_01", "Personal", "version_old", []contracts.CapsuleRef{{Ref: testCapsuleRefB, FreshnessPolicy: contracts.Pin}})
	if err := store.SaveAuthoringProfile(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := fixture.application.runProfile(context.Background(), []string{"publish"}); err != nil {
		t.Fatal(err)
	}
	after, found, err := store.ReadAuthoringProfile("profile_01")
	if err != nil || !found || after.LastObservedHeadVersionID != "version_new" || len(after.CapsuleRefs) != 2 || after.CapsuleRefs[1].Ref != testCapsuleRefC {
		t.Fatalf("post-publish state = %#v found=%t err=%v", after, found, err)
	}
}

func TestProfileRefreshKeepsPendingDraftForksItLocallyAndCanReselect(t *testing.T) {
	var accessToken string
	var forkBody contracts.PublishProfileVersionJSONRequestBody
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/me":
			_ = json.NewEncoder(response).Encode(contracts.User{Id: "user-1", DefaultRegion: "us-east-1"})
		case "/v1/profile-versions/version_old":
			_ = json.NewEncoder(response).Encode(profileVersion("version_old", "profile_01", 1, testCapsuleRefA))
		case "/v1/profiles/profile_01":
			head := "version_current"
			_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_01", Name: "Personal", Slug: "personal", HeadVersionId: &head})
		case "/v1/profile-versions/version_current":
			_ = json.NewEncoder(response).Encode(profileVersion("version_current", "profile_01", 2, testCapsuleRefC))
		case "/v1/profiles":
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_02", Name: "Draft Fork", Slug: "draft-fork"})
		case "/v1/profiles/profile_02/versions":
			if err := json.NewDecoder(request.Body).Decode(&forkBody); err != nil {
				t.Error(err)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(profileVersion("version_fork", "profile_02", 1, testCapsuleRefB))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	store := newLocalStateStore(fixture.configDirectory)
	localDraft := authoringProfileFromContracts("profile_01", "Personal", "version_old", []contracts.CapsuleRef{{Ref: testCapsuleRefB, FreshnessPolicy: contracts.Pin}})
	if err := store.SaveAuthoringProfile(context.Background(), localDraft); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	fixture.application.output = &output
	if err := fixture.application.runProfile(context.Background(), []string{"refresh"}); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.ReadSelectedAuthoringProfile()
	if err != nil || refreshed.LastObservedHeadVersionID != "version_current" || len(refreshed.CapsuleRefs) != 1 || refreshed.CapsuleRefs[0].Ref != testCapsuleRefB || !strings.Contains(output.String(), "remain pending") {
		t.Fatalf("refreshed = %#v output=%q err=%v", refreshed, output.String(), err)
	}
	if err := fixture.application.runProfile(context.Background(), []string{"fork", "--local", "Draft Fork"}); err != nil {
		t.Fatal(err)
	}
	if forkBody.ExpectedHeadVersionId != nil || len(forkBody.CapsuleRefs) != 1 || forkBody.CapsuleRefs[0].Ref != testCapsuleRefB {
		t.Fatalf("local fork body = %#v", forkBody)
	}
	if err := fixture.application.runProfile(context.Background(), []string{"select", "profile_01"}); err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReadSelectedAuthoringProfile()
	if err != nil || selected.ProfileID != "profile_01" {
		t.Fatalf("reselected = %#v err=%v", selected, err)
	}
}

func TestProfilePublishRejectsForeignOwnerBeforePost(t *testing.T) {
	var accessToken string
	var publishCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/v1/me" {
			_ = json.NewEncoder(response).Encode(contracts.User{Id: "user-1", DefaultRegion: "us-east-1"})
			return
		}
		publishCalls.Add(1)
		http.NotFound(response, request)
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	foreign := "owner/user-2/capsule@sha256:" + strings.Repeat("d", 64)
	record := authoringProfileFromContracts("profile_01", "Personal", "", []contracts.CapsuleRef{{Ref: foreign, FreshnessPolicy: contracts.Pin}})
	if err := newLocalStateStore(fixture.configDirectory).SaveAuthoringProfile(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	err := fixture.application.runProfile(context.Background(), []string{"publish"})
	if err == nil || !strings.Contains(err.Error(), "belongs to a different owner") || publishCalls.Load() != 0 {
		t.Fatalf("foreign-owner publish err=%v calls=%d", err, publishCalls.Load())
	}
}

func TestProfileAddRejectsGiantAndDuplicateExclusionsWithoutLockout(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "devm")
	store := newLocalStateStore(directory)
	record := authoringProfileFromContracts("profile_01", "Personal", "", []contracts.CapsuleRef{{Ref: testCapsuleRefA, FreshnessPolicy: contracts.Track}})
	if err := store.SaveAuthoringProfile(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	application := cli{configDirectory: func() (string, error) { return directory, nil }}
	for _, arguments := range [][]string{
		{"add", testCapsuleRefB, "--exclude", strings.Repeat("x", maxLocalStateFileSize+1)},
		{"add", testCapsuleRefB, "--exclude", "config:editor", "--exclude", "config:editor"},
	} {
		if err := application.runProfile(context.Background(), arguments); err == nil {
			t.Fatalf("invalid exclusions %v were accepted", arguments)
		}
		selected, readErr := store.ReadSelectedAuthoringProfile()
		if readErr != nil || !reflect.DeepEqual(selected, record) {
			t.Fatalf("invalid add locked out record: selected=%#v err=%v", selected, readErr)
		}
	}
	err := store.UpdateSelectedAuthoringProfile(context.Background(), func(profile *localAuthoringProfile) error {
		profile.CapsuleRefs = make([]localCapsuleRef, 6000)
		for index := range profile.CapsuleRefs {
			profile.CapsuleRefs[index] = localCapsuleRef{Ref: fmt.Sprintf("owner/user-1/capsule:t%06d", index), FreshnessPolicy: "track"}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized encoded record error = %v", err)
	}
	selected, readErr := store.ReadSelectedAuthoringProfile()
	if readErr != nil || !reflect.DeepEqual(selected, record) {
		t.Fatalf("oversized encoded record replaced state: selected=%#v err=%v", selected, readErr)
	}
}

func TestProfileForkResumesAfterCreateBeforePublishFailure(t *testing.T) {
	var accessToken string
	var createKeys, publishKeys []string
	var publishCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/profile-versions/version_source":
			_ = json.NewEncoder(response).Encode(profileVersion("version_source", "profile_source", 1, testCapsuleRefB))
		case "/v1/me":
			_ = json.NewEncoder(response).Encode(contracts.User{Id: "user-1", DefaultRegion: "us-east-1"})
		case "/v1/profiles":
			createKeys = append(createKeys, request.Header.Get("Idempotency-Key"))
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(contracts.ProfileSummary{Id: "profile_02", Name: "Forked", Slug: "forked"})
		case "/v1/profiles/profile_02/versions":
			publishKeys = append(publishKeys, request.Header.Get("Idempotency-Key"))
			if publishCalls.Add(1) == 1 {
				response.WriteHeader(http.StatusServiceUnavailable)
				_, _ = response.Write([]byte(`{"error":{"code":"COMMAND_UNAVAILABLE","message":"peer text"},"requestId":"request-1"}`))
				return
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(profileVersion("version_fork", "profile_02", 1, testCapsuleRefB))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	arguments := []string{"fork", "version_source", "Forked"}
	if err := fixture.application.runProfile(context.Background(), arguments); err == nil || !strings.Contains(err.Error(), "retry later") {
		t.Fatalf("first fork error = %v", err)
	}
	if _, found, err := newLocalStateStore(fixture.configDirectory).ReadAuthoringProfile("profile_02"); err != nil || found {
		t.Fatalf("partial fork was persisted: found=%t err=%v", found, err)
	}
	if err := fixture.application.runProfile(context.Background(), arguments); err != nil {
		t.Fatal(err)
	}
	if len(createKeys) != 2 || createKeys[0] == "" || createKeys[0] != createKeys[1] || len(publishKeys) != 2 || publishKeys[0] == "" || publishKeys[0] != publishKeys[1] {
		t.Fatalf("fork retry keys create=%v publish=%v", createKeys, publishKeys)
	}
	selected, err := newLocalStateStore(fixture.configDirectory).ReadSelectedAuthoringProfile()
	if err != nil || selected.ProfileID != "profile_02" || selected.LastObservedHeadVersionID != "version_fork" {
		t.Fatalf("resumed fork = %#v err=%v", selected, err)
	}
}

func TestProfileApplyMapsNotImplementedAndProfileErrorsWithoutPeerText(t *testing.T) {
	var accessToken string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertProfileAuthorization(t, request, accessToken)
		if request.URL.Path == "/v1/environments/env_01/apply-profile" {
			response.WriteHeader(http.StatusNotImplemented)
			return
		}
		if request.URL.Path == "/v1/profiles" {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":{"code":"PROFILE_CONFLICT","message":"` + profilePeerSecret + `"},"requestId":"request-1"}`))
			return
		}
		http.NotFound(response, request)
	}))
	defer server.Close()
	fixture := newLifecycleFixture(t, server)
	accessToken = fixture.accessToken
	if err := newLocalStateStore(fixture.configDirectory).BindProject(context.Background(), "git://example.test/owner/repo", "env_01"); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.application.workingDirectory = func() (string, error) { return repository, nil }
	fixture.application.git = lifecycleGitFake(repository, "git@example.test:owner/repo.git")
	if err := fixture.application.runProfile(context.Background(), []string{"apply", "version_01"}); err == nil || err.Error() != "profile apply is not yet available on this control plane" {
		t.Fatalf("501 error = %v", err)
	}
	err := fixture.application.runProfile(context.Background(), []string{"create", "Personal"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), profilePeerSecret) {
		t.Fatalf("mapped Profile conflict = %v", err)
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
