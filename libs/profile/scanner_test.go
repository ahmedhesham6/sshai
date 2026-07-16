package profile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestScanClassifiesOnlyKnownProfileCandidatesDeterministically(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, "AGENTS.md", "Use Go.\n", 0o755)
	writeProfileFile(t, root, ".codex/skills/review/SKILL.md", "# Review\n", 0o644)
	writeProfileFile(t, root, ".codex/skills/review/scripts/check.sh", "#!/bin/sh\ntouch must-not-run\n", 0o755)
	writeProfileFile(t, root, ".codex/mystery.txt", "unknown body must stay unread by consumers\n", 0o644)
	writeProfileFile(t, root, ".codex/auth.json", "{\"access_token\":\"must-not-leak\"}\n", 0o000)

	first, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("scan Profile candidates: %v", err)
	}
	second, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("scan Profile candidates again: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("candidate count changed: %d != %d", len(first), len(second))
	}
	want := []struct {
		path, kind, evidence, sensitivity, trust, disposition string
		executable                                            bool
	}{
		{".codex/auth.json", "auth_cache", "known_auth_cache", "credential", "unknown", "excluded", false},
		{".codex/mystery.txt", "unknown", "unknown_file_in_known_root", "unknown", "unknown", "excluded", false},
		{".codex/skills/review/SKILL.md", "agent_skill_instruction", "open_agent_skill_manifest", "private", "third_party", "safe", false},
		{".codex/skills/review/scripts/check.sh", "agent_skill_executable", "open_agent_skill_script+executable_mode", "private", "third_party", "review", true},
		{"AGENTS.md", "agent_instruction", "known_agent_instruction+executable_mode", "private", "user_authored", "review", false},
	}
	if len(first) != len(want) {
		t.Fatalf("candidates = %+v", first)
	}
	for i, expected := range want {
		candidate := first[i]
		if candidate.Path != expected.path || candidate.Kind != expected.kind || candidate.Evidence != expected.evidence || candidate.Sensitivity != expected.sensitivity || candidate.Trust != expected.trust || candidate.Disposition != expected.disposition || candidate.ContainsExecutable != expected.executable {
			t.Fatalf("candidate %d = %+v", i, candidate)
		}
		if candidate.Path != ".codex/auth.json" && candidate.Path != ".codex/mystery.txt" && (candidate.SourceDigest == "" || candidate.ContentDigest == "") {
			t.Fatalf("portable candidate lacks digests: %+v", candidate)
		}
		if candidate.SourceLocator != candidate.Path+"#$" {
			t.Fatalf("source locator = %q", candidate.SourceLocator)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "must-not-run")); !os.IsNotExist(err) {
		t.Fatal("scanner executed selected skill content")
	}
}

func TestScanUsesTypedComponentKindsAndExecutablePolicy(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, "AGENTS.md", "Use Go.\n", 0o644)
	writeProfileFile(t, root, ".codex/config.toml", "model = \"gpt\"\n", 0o644)
	writeProfileFile(t, root, ".claude/settings.json", "{\"theme\":\"dark\"}\n", 0o644)
	writeProfileFile(t, root, ".bashrc", "alias ll='ls -la'\n", 0o644)
	writeProfileFile(t, root, ".gitconfig", "[user]\nname = Test\n", 0o644)
	writeProfileFile(t, root, ".codex/skills/review/SKILL.md", "# Review\n", 0o644)
	writeProfileFile(t, root, ".codex/skills/review/scripts/check.sh", "#!/bin/sh\nexit 0\n", 0o644)

	candidates, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("scan Profile candidates: %v", err)
	}
	want := map[string]bool{
		"agent_instruction": false, "codex_settings": false, "claude_settings": false,
		"shell_preferences": true, "git_preferences": false,
		"agent_skill_instruction": false, "agent_skill_executable": true,
	}
	if len(candidates) != len(want) {
		t.Fatalf("candidates = %+v", candidates)
	}
	for _, candidate := range candidates {
		executable, ok := want[candidate.Kind]
		if !ok || candidate.ContainsExecutable != executable {
			t.Fatalf("candidate classification = %+v", candidate)
		}
		delete(want, candidate.Kind)
	}
	if len(want) != 0 {
		t.Fatalf("missing candidate kinds = %v", want)
	}
}

func TestScanRejectsKnownCandidateSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(outside, []byte("model = \"secret\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".codex", "config.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Scan(root); err == nil {
		t.Fatal("scanner accepted a known candidate symlink escape")
	}
}

func TestScanRejectsKnownRootSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "config.toml"), []byte("model = \"safe\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".codex")); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Scan(root); err == nil {
		t.Fatal("scanner accepted a known-root symlink escape")
	}
}

func TestScanExcludesCredentialBearingKnownCandidate(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, "AGENTS.md", "API_TOKEN=credential-value\n", 0o644)
	candidates, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("scan Profile candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Disposition != "excluded" || candidates[0].Sensitivity != "credential" || candidates[0].ContentDigest != "" {
		t.Fatalf("credential candidate = %+v", candidates)
	}
}

func TestScanNeverPackagesSensitiveAndUnknownPaths(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, "AGENTS.md", "Use Go.\n", 0o644)
	for _, path := range []string{
		".codex/auth.json",
		".codex/sessions/2026/session.jsonl",
		".claude/.credentials.json",
		".claude/projects/repo/session.jsonl",
		".config/opencode/auth.json",
		".config/opencode/mcp-auth.json",
		".local/share/opencode/auth.json",
		".local/share/opencode/mcp-auth.json",
		".opencode/auth.json",
		".opencode/mcp-auth.json",
		".ssh/id_ed25519",
		".codex/unknown.dat",
	} {
		writeProfileFile(t, root, path, "secret body must not be read or packaged\n", 0o600)
	}

	candidates, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	byPath := make(map[string]profile.Candidate, len(candidates))
	for _, candidate := range candidates {
		byPath[candidate.Path] = candidate
	}
	for _, path := range []string{
		".codex/auth.json",
		".codex/sessions/2026/session.jsonl",
		".claude/.credentials.json",
		".claude/projects/repo/session.jsonl",
		".config/opencode/auth.json",
		".config/opencode/mcp-auth.json",
		".local/share/opencode/auth.json",
		".local/share/opencode/mcp-auth.json",
		".opencode/auth.json",
		".opencode/mcp-auth.json",
		".ssh/id_ed25519",
		".codex/unknown.dat",
	} {
		candidate, ok := byPath[path]
		if !ok {
			t.Fatalf("Scan() omitted never-package path %q", path)
		}
		if candidate.Disposition != "excluded" || candidate.ContentDigest != "" || candidate.SourceDigest != "" {
			t.Fatalf("never-package candidate %q = %+v", path, candidate)
		}
	}
	portable := byPath["AGENTS.md"]
	if portable.Component.Type != domain.ComponentConfig || portable.Component.Scope != domain.ScopeUser || portable.Component.TrustClass != domain.TrustDeclarative {
		t.Fatalf("portable component mapping = %+v", portable.Component)
	}
	if portable.Component.ID != "config:AGENTS.md" {
		t.Fatalf("portable component ID = %q", portable.Component.ID)
	}
}

func TestScanExtractsMCPSecretNamesAndBlocksLiteralValues(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, ".codex/config.toml", "[mcp_servers.github.env]\nGITHUB_TOKEN = \"${GITHUB_TOKEN}\"\nGITHUB_ORG = \"literal-secret-value\"\n", 0o600)
	writeProfileFile(t, root, ".claude/settings.json", `{"mcpServers":{"docs":{"command":"docs-server"}}}`+"\n", 0o600)
	writeProfileFile(t, root, ".config/opencode/opencode.json", `{
  "mcp": {
    "docs": {
      "environment": {
        "DOCS_TOKEN": "{env:DOCS_TOKEN}"
      }
    }
  }
}`+"\n", 0o600)

	candidates, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	byPath := make(map[string]profile.Candidate, len(candidates))
	for _, candidate := range candidates {
		byPath[candidate.Path] = candidate
	}
	literal := byPath[".codex/config.toml"]
	if literal.Component.Type != domain.ComponentIntegration || literal.Disposition != "excluded" || literal.ContentDigest != "" {
		t.Fatalf("literal MCP candidate = %+v", literal)
	}
	if got := literal.Component.Requirements.Secrets; len(got) != 2 || got[0] != "GITHUB_ORG" || got[1] != "GITHUB_TOKEN" {
		t.Fatalf("literal MCP secret requirements = %#v", got)
	}
	if strings.Contains(literal.Evidence, "literal-secret-value") {
		t.Fatalf("MCP evidence leaked value: %q", literal.Evidence)
	}

	referenced := byPath[".config/opencode/opencode.json"]
	if referenced.Component.Type != domain.ComponentIntegration || referenced.Disposition != "requires_authorization" {
		t.Fatalf("referenced MCP candidate = %+v", referenced)
	}
	if got := referenced.Component.Requirements.Secrets; len(got) != 1 || got[0] != "DOCS_TOKEN" {
		t.Fatalf("referenced MCP secret requirements = %#v", got)
	}
	if got := byPath[".claude/settings.json"].Component.Type; got != domain.ComponentIntegration {
		t.Fatalf("MCP reference without env mapped to %q", got)
	}
}

func TestScanPlacesExecutableMCPReferenceInRequiresAuthorization(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, ".mcp.json", `{"mcpServers":{"docs":{"command":"docs-server"}}}`+"\n", 0o755)

	candidates, err := profile.Scan(root)
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v", candidates)
	}
	candidate := candidates[0]
	if candidate.Disposition != "requires_authorization" {
		t.Fatalf("executable MCP disposition = %q, want requires_authorization: %+v", candidate.Disposition, candidate)
	}
	if !candidate.ContainsExecutable {
		t.Fatalf("executable MCP candidate did not retain executable flag: %+v", candidate)
	}
}

func writeProfileFile(t *testing.T, root, path, content string, mode os.FileMode) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
