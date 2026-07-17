package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
