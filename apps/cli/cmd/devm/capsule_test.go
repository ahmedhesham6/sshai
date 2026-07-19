package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestRunCapsuleCapturePrintsComponentRiskGroups(t *testing.T) {
	root := t.TempDir()
	writeCLIProfileFile(t, root, "AGENTS.md", "Use Go.\n", 0o644)
	writeCLIProfileFile(t, root, ".bashrc", "alias ll='ls -la'\n", 0o644)
	writeCLIProfileFile(t, root, ".codex/unknown.txt", "unknown content must not print\n", 0o600)

	var output bytes.Buffer
	if err := RunCapsuleCapture(context.Background(), root, []profile.Selector{{Path: "AGENTS.md", Selector: "$"}}, &output); err != nil {
		t.Fatalf("RunCapsuleCapture(): %v", err)
	}
	for _, expected := range []string{
		"safe:\n",
		"component=config:AGENTS.md type=config",
		"review:\n",
		"component=config:.bashrc type=config",
		"requires_authorization:\n",
		"excluded:\n",
		"component=config:.codex/unknown.txt type=config",
		"conflict:\n",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("capture output lacks %q:\n%s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "unknown content must not print") {
		t.Fatal("capture output exposed excluded content")
	}
}

func TestRunCapsuleBuildPrintsManifestAndComponentDigests(t *testing.T) {
	root := t.TempDir()
	writeCLIProfileFile(t, root, "AGENTS.md", "Use Go.\n", 0o644)
	writeCLIProfileFile(t, root, ".claude/settings.json", `{"theme":"dark"}`+"\n", 0o600)
	selectors := []profile.Selector{
		{Path: ".claude/settings.json", Selector: "$.theme"},
		{Path: "AGENTS.md", Selector: "$"},
	}

	var first bytes.Buffer
	if err := RunCapsuleBuild(context.Background(), root, selectors, &first); err != nil {
		t.Fatalf("RunCapsuleBuild(): %v", err)
	}
	var second bytes.Buffer
	if err := RunCapsuleBuild(context.Background(), root, []profile.Selector{selectors[1], selectors[0]}, &second); err != nil {
		t.Fatalf("RunCapsuleBuild() reordered selectors: %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("build output changed with selector order:\n%s\n%s", first.String(), second.String())
	}
	for _, expected := range []string{
		"manifest_digest sha256:",
		"component id=\"config:.claude/settings.json#$.theme\" digest=sha256:",
		"component id=\"config:AGENTS.md\" digest=sha256:",
	} {
		if !strings.Contains(first.String(), expected) {
			t.Fatalf("build output lacks %q:\n%s", expected, first.String())
		}
	}
}

func TestCLIRoutesCapsuleBuildFromSelectionsFile(t *testing.T) {
	root := t.TempDir()
	writeCLIProfileFile(t, root, "AGENTS.md", "Use Go.\n", 0o644)
	selectionsPath := filepath.Join(t.TempDir(), "selections.json")
	if err := os.WriteFile(selectionsPath, []byte(`[{"path":"AGENTS.md","selector":"$"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	application := cli{output: &output}
	if err := application.run(context.Background(), []string{"capsule", "build", "--profile-root", root, "--selections", selectionsPath}); err != nil {
		t.Fatalf("route capsule build: %v", err)
	}
	if !strings.Contains(output.String(), "manifest_digest sha256:") || !strings.Contains(output.String(), `component id="config:AGENTS.md" digest=sha256:`) {
		t.Fatalf("capsule route output = %s", output.String())
	}
}

func TestCapsulePublishInspectAndComponentDiffThroughHTTPGrants(t *testing.T) {
	store := newCapsuleHTTPStore(t)
	api, err := contracts.NewClientWithResponses(store.server.URL, contracts.WithHTTPClient(store.server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	provider := capsuleGrantProvider{api: api, httpClient: store.server.Client(), token: "access-token"}
	root := t.TempDir()
	writeCLIProfileFile(t, root, "AGENTS.md", "Use Go.\n", 0o644)
	selectors := []profile.Selector{{Path: "AGENTS.md", Selector: "$"}}
	var output bytes.Buffer
	command := capsuleRemoteCommand{api: api, grants: provider, ownerID: "user-1", token: "access-token", output: &output}
	if err := command.publish(t.Context(), "agents:stable", root, selectors); err != nil {
		t.Fatalf("publish stable: %v", err)
	}
	stableDigest := store.tagDigest("agents", "stable")
	if stableDigest == "" || !strings.Contains(output.String(), contracts.FormatOwnedCapsuleTagRef("user-1", "agents", "stable")) {
		t.Fatalf("publish output/tag = %q / %q", output.String(), stableDigest)
	}
	output.Reset()
	if err := command.publish(t.Context(), "agents:stable", root, selectors); err != nil {
		t.Fatalf("idempotent publish stable: %v", err)
	}

	output.Reset()
	if err := command.inspect(t.Context(), "agents:stable"); err != nil {
		t.Fatalf("inspect stable: %v", err)
	}
	if !strings.Contains(output.String(), "manifest name=\"agents\"") || !strings.Contains(output.String(), "component id=\"config:AGENTS.md\"") {
		t.Fatalf("inspect output = %s", output.String())
	}

	writeCLIProfileFile(t, root, "AGENTS.md", "Use Go and run tests.\n", 0o644)
	output.Reset()
	if err := command.publish(t.Context(), "agents:next", root, selectors); err != nil {
		t.Fatalf("publish next: %v", err)
	}
	output.Reset()
	if err := command.diff(t.Context(), "agents:stable", "agents:next"); err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(output.String(), "changed id=\"config:AGENTS.md\" type=config scope=user from=sha256:") {
		t.Fatalf("Component diff output = %s", output.String())
	}
}

func TestCapsuleGrantErrorsDoNotLeakCapabilityURLs(t *testing.T) {
	store := newCapsuleHTTPStore(t)
	store.failReads = true
	api, err := contracts.NewClientWithResponses(store.server.URL, contracts.WithHTTPClient(store.server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	provider := capsuleGrantProvider{api: api, httpClient: store.server.Client(), token: "access-token"}
	grant, err := provider.Grant(t.Context(), oci.GrantRequest{
		OwnerID: "user-1", Key: oci.IndexKey("user-1", "sha256:"+strings.Repeat("a", 64)), Operation: oci.GrantRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = grant.Read(t.Context())
	if err == nil || strings.Contains(err.Error(), "CAPABILITY_SECRET") || strings.Contains(err.Error(), store.server.URL) {
		t.Fatalf("read error leaked capability URL: %v", err)
	}
}

type capsuleHTTPStore struct {
	server    *httptest.Server
	mu        sync.Mutex
	objects   map[string][]byte
	tags      map[string]contracts.CapsuleTag
	failReads bool
}

func newCapsuleHTTPStore(t *testing.T) *capsuleHTTPStore {
	t.Helper()
	store := &capsuleHTTPStore{objects: make(map[string][]byte), tags: make(map[string]contracts.CapsuleTag)}
	store.server = httptest.NewServer(http.HandlerFunc(store.serveHTTP))
	t.Cleanup(store.server.Close)
	return store
}

func (store *capsuleHTTPStore) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/objects/") {
		store.serveObject(response, request)
		return
	}
	if request.Header.Get("Authorization") != "Bearer access-token" {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/capsule-access":
		var body contracts.CapsuleAccessRequest
		if json.NewDecoder(request.Body).Decode(&body) != nil || len(body.Objects) != 1 {
			http.Error(response, "bad request", http.StatusBadRequest)
			return
		}
		object := body.Objects[0]
		method := contracts.GET
		if body.Intent == contracts.Push {
			method = contracts.PUT
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(contracts.CapsuleAccessResponse{Grants: []contracts.CapsuleAccessGrant{{
			Url:    store.server.URL + "/objects/" + string(object.Kind) + "/" + object.Digest + "?token=CAPABILITY_SECRET",
			Method: method, Headers: map[string]string{}, ExpiresAt: time.Now().Add(time.Hour),
		}}})
	case strings.HasPrefix(request.URL.Path, "/capsules/"):
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "capsules" || parts[2] != "tags" {
			http.NotFound(response, request)
			return
		}
		key := parts[1] + ":" + parts[3]
		store.mu.Lock()
		defer store.mu.Unlock()
		if request.Method == http.MethodPut {
			var body contracts.PutCapsuleTagRequest
			if json.NewDecoder(request.Body).Decode(&body) != nil {
				http.Error(response, "bad request", http.StatusBadRequest)
				return
			}
			if existing, exists := store.tags[key]; !exists || existing.Digest != body.Digest {
				store.tags[key] = contracts.CapsuleTag{Name: parts[1], Tag: parts[3], Digest: body.Digest, UpdatedAt: time.Now().UTC()}
			}
		}
		record, exists := store.tags[key]
		if !exists {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(record)
	default:
		http.NotFound(response, request)
	}
}

func (store *capsuleHTTPStore) serveObject(response http.ResponseWriter, request *http.Request) {
	key := strings.TrimPrefix(request.URL.Path, "/objects/")
	store.mu.Lock()
	defer store.mu.Unlock()
	if request.Method == http.MethodPut {
		if _, exists := store.objects[key]; exists {
			response.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		content, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(response, "read", http.StatusBadRequest)
			return
		}
		store.objects[key] = content
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if store.failReads {
		http.Error(response, "CAPABILITY_SECRET", http.StatusInternalServerError)
		return
	}
	content, exists := store.objects[key]
	if !exists {
		http.NotFound(response, request)
		return
	}
	_, _ = response.Write(content)
}

func (store *capsuleHTTPStore) tagDigest(name, tag string) string {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.tags[name+":"+tag].Digest
}

func TestCaptureAndPlanRenderExecutableContentFlag(t *testing.T) {
	profileRoot := t.TempDir()
	writeCLIProfileFile(t, profileRoot, ".mcp.json", `{"mcpServers":{"docs":{"command":"docs-server"}}}`+"\n", 0o755)

	var capture bytes.Buffer
	if err := RunCapsuleCapture(context.Background(), profileRoot, nil, &capture); err != nil {
		t.Fatalf("RunCapsuleCapture(): %v", err)
	}
	if !strings.Contains(capture.String(), "contains_executable=true") {
		t.Fatalf("capture output omitted executable-content flag:\n%s", capture.String())
	}

	repository := planRepository(t)
	var plan bytes.Buffer
	if err := RunPlan(context.Background(), repository, profileRoot, nil, &plan); err != nil {
		t.Fatalf("RunPlan(): %v", err)
	}
	if !strings.Contains(plan.String(), "contains_executable=true") {
		t.Fatalf("plan output omitted executable-content flag:\n%s", plan.String())
	}
}

func writeCLIProfileFile(t *testing.T, root, path, content string, mode os.FileMode) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
