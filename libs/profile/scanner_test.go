package profile_test

import (
	"os"
	"path/filepath"
	"testing"

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

func TestCompileUsesClosedExecutablePolicyInsteadOfFileMode(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, "AGENTS.md", "Use Go.\n", 0o755)
	writeProfileFile(t, root, ".bashrc", "alias ll='ls -la'\n", 0o644)
	writeProfileFile(t, root, ".codex/skills/review/scripts/check.sh", "#!/bin/sh\nexit 0\n", 0o644)

	version, err := profile.Compile(root, []profile.Selector{
		{Path: "AGENTS.md", Selector: "$"},
		{Path: ".bashrc", Selector: "$"},
		{Path: ".codex/skills/review/scripts/check.sh", Selector: "$"},
	})
	if err != nil {
		t.Fatalf("compile Profile Version: %v", err)
	}
	artifacts := version.Artifacts()
	want := []struct {
		kind       string
		executable bool
	}{
		{"shell_preferences", true},
		{"agent_skill_executable", true},
		{"agent_instruction", false},
	}
	if len(artifacts) != len(want) {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	for i, expected := range want {
		if artifacts[i].Kind != expected.kind || artifacts[i].ContainsExecutable != expected.executable {
			t.Fatalf("artifact %d classification = kind:%q executable:%t", i, artifacts[i].Kind, artifacts[i].ContainsExecutable)
		}
	}
	if artifacts[2].Evidence != "known_agent_instruction+executable_mode" {
		t.Fatalf("executable-mode evidence = %q", artifacts[2].Evidence)
	}
}

func TestScanUsesExactlyTheDomainArtifactKindsAndExecutablePolicy(t *testing.T) {
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
		t.Fatalf("missing artifact kinds = %v", want)
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
