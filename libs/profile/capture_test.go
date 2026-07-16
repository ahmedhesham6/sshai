package profile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestCompileBuildsDeterministicCapsuleComponents(t *testing.T) {
	root := t.TempDir()
	writeCaptureFile(t, root, "AGENTS.md", []byte("Use Go.\n"), 0o644)
	writeCaptureFile(t, root, ".claude/settings.json", []byte(`{"theme":"dark"}`), 0o600)
	selectors := []profile.Selector{
		{Path: ".claude/settings.json", Selector: "$.theme"},
		{Path: "AGENTS.md", Selector: "$"},
	}

	first, err := profile.Compile(root, selectors)
	if err != nil {
		t.Fatalf("Compile(): %v", err)
	}
	second, err := profile.Compile(root, []profile.Selector{selectors[1], selectors[0]})
	if err != nil {
		t.Fatalf("Compile() reordered selectors: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("capsule digest changed with selector order: %q != %q", first.Digest, second.Digest)
	}
	if first.Manifest.Name != "captured-profile" {
		t.Fatalf("capsule name = %q", first.Manifest.Name)
	}
	if len(first.Manifest.Components) != 2 || len(first.Layers) != 2 {
		t.Fatalf("capsule components/layers = %d/%d", len(first.Manifest.Components), len(first.Layers))
	}

	settings := first.Manifest.Components[0]
	if settings.ID != "config:.claude/settings.json#$.theme" || settings.Type != capsule.ComponentTypeConfig || settings.Scope != capsule.ScopeUser || settings.TrustClass != capsule.TrustDeclarative {
		t.Fatalf("settings component = %+v", settings)
	}
	if settings.Digest == "" || settings.MediaType == "" || settings.SizeBytes <= 0 {
		t.Fatalf("settings component lacks layer descriptor = %+v", settings)
	}
	if got := first.Layers[0].Index[0].Digest; got != "sha256:7b9365c5e1321c0b8b2e1c980c2859b4515849c848e059f6789ff9ab5c89b2d3" {
		t.Fatalf("selected JSON fragment digest = %q", got)
	}
	if first.Layers[0].ComponentID != settings.ID {
		t.Fatalf("settings layer component ID = %q", first.Layers[0].ComponentID)
	}

	instructions := first.Manifest.Components[1]
	if instructions.ID != "config:AGENTS.md" || instructions.Type != capsule.ComponentTypeConfig || instructions.Scope != capsule.ScopeUser || instructions.TrustClass != capsule.TrustDeclarative {
		t.Fatalf("instructions component = %+v", instructions)
	}
	if err := (domain.Component{
		ID: instructions.ID, Type: domain.ComponentType(instructions.Type), MediaType: instructions.MediaType,
		Digest: instructions.Digest, SizeBytes: instructions.SizeBytes, Scope: domain.ComponentScope(instructions.Scope),
		TrustClass: domain.TrustClass(instructions.TrustClass),
	}).Validate(); err != nil {
		t.Fatalf("compiled component does not honor domain mapping: %v", err)
	}
}

func TestCompileRejectsNeverPackageAndCredentialBearingSources(t *testing.T) {
	root := t.TempDir()
	writeCaptureFile(t, root, ".codex/auth.json", []byte(`{"access_token":"must-not-leak"}`), 0o600)
	writeCaptureFile(t, root, ".codex/unknown.txt", []byte("unknown"), 0o600)
	writeCaptureFile(t, root, ".ssh/id_ed25519", []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600)
	writeCaptureFile(t, root, "AGENTS.md", []byte("API_TOKEN=credential-value\n"), 0o600)
	for _, selector := range []profile.Selector{
		{Path: ".codex/auth.json", Selector: "$"},
		{Path: ".codex/unknown.txt", Selector: "$"},
		{Path: ".ssh/id_ed25519", Selector: "$"},
		{Path: "AGENTS.md", Selector: "$"},
	} {
		if _, err := profile.Compile(root, []profile.Selector{selector}); err == nil {
			t.Fatalf("Compile() accepted never-package selector %+v", selector)
		}
	}
}

func TestSymlinkToAuthCacheFailsClosedDuringScanAndCompile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := `{"access_token":"symlinked-access-token"}`
	if err := os.WriteFile(filepath.Join(root, ".codex", "auth.json"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("auth.json", filepath.Join(root, ".codex", "config.toml")); err != nil {
		t.Fatal(err)
	}

	candidates, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	var linked profile.Candidate
	for _, candidate := range candidates {
		if candidate.Path == ".codex/config.toml" {
			linked = candidate
			break
		}
	}
	if linked.Path == "" {
		t.Fatalf("Scan() omitted symlink candidate: %+v", candidates)
	}
	if linked.Disposition != "excluded" {
		t.Fatalf("symlink candidate disposition = %q, want excluded: %+v", linked.Disposition, linked)
	}
	if !strings.Contains(linked.Evidence, ".codex/config.toml") || !strings.Contains(linked.Evidence, ".codex/auth.json") {
		t.Fatalf("symlink evidence = %q, want both paths", linked.Evidence)
	}
	if strings.Contains(linked.Evidence, "symlinked-access-token") {
		t.Fatalf("symlink evidence leaked secret: %q", linked.Evidence)
	}

	if _, err := profile.Compile(root, []profile.Selector{{Path: ".codex/config.toml", Selector: "$"}}); err == nil {
		t.Fatal("Compile() accepted symlink to never-package auth cache")
	} else if strings.Contains(err.Error(), "symlinked-access-token") {
		t.Fatalf("Compile() error leaked secret: %v", err)
	}
}

func TestCompileBlocksQuotedJSONCredentialAfterSelectorWithoutEchoingValue(t *testing.T) {
	root := t.TempDir()
	secret := "quoted-json-access-token-value-123456789"
	writeCaptureFile(t, root, ".claude/settings.json", []byte(`{"server":{"apiKey":"`+secret+`"},"theme":"dark"}`), 0o600)

	candidates, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	var scanned profile.Candidate
	for _, candidate := range candidates {
		if candidate.Path == ".claude/settings.json" {
			scanned = candidate
			break
		}
	}
	if scanned.Path == "" {
		t.Fatalf("Scan() omitted settings candidate: %+v", candidates)
	}
	if strings.Contains(scanned.Evidence, secret) {
		t.Fatalf("Scan() evidence leaked secret: %q", scanned.Evidence)
	}

	_, err = profile.Compile(root, []profile.Selector{{Path: ".claude/settings.json", Selector: "$.server"}})
	if err == nil {
		t.Fatal("Compile() accepted quoted JSON credential in selected content")
	}
	if !strings.Contains(err.Error(), "apiKey") {
		t.Fatalf("Compile() error = %v, want credential key evidence", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Compile() error leaked secret: %v", err)
	}
}

func TestCompileExtractsSelectorFromJSONCWithComments(t *testing.T) {
	root := t.TempDir()
	writeCaptureFile(t, root, "opencode.jsonc", []byte(`{
  // Keep this comment out of the canonical selected fragment.
  "mcp": {
    /* The server declaration is still selectable. */
    "docs": {"command": "docs-server"}
  }
}`), 0o600)

	result, err := profile.Compile(root, []profile.Selector{{Path: "opencode.jsonc", Selector: "$.mcp"}})
	if err != nil {
		t.Fatalf("Compile() JSONC selector: %v", err)
	}
	if len(result.Manifest.Components) != 1 {
		t.Fatalf("compiled JSONC components = %d, want 1", len(result.Manifest.Components))
	}
}

func TestCompileCarriesMCPSecretRequirementsWithoutValues(t *testing.T) {
	root := t.TempDir()
	content := []byte("[mcp_servers.github.env]\nGITHUB_TOKEN = \"${GITHUB_TOKEN}\"\n")
	writeCaptureFile(t, root, ".codex/config.toml", content, 0o600)
	result, err := profile.Compile(root, []profile.Selector{{Path: ".codex/config.toml", Selector: "$"}})
	if err != nil {
		t.Fatalf("Compile(): %v", err)
	}
	component := result.Manifest.Components[0]
	if component.Type != capsule.ComponentTypeIntegration || len(component.Requirements.Secrets) != 1 || component.Requirements.Secrets[0] != "GITHUB_TOKEN" {
		t.Fatalf("MCP component = %+v", component)
	}
}

func writeCaptureFile(t *testing.T, root, path string, content []byte, mode os.FileMode) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, content, mode); err != nil {
		t.Fatal(err)
	}
}
