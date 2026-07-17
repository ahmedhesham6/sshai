package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestRunPlanComposesDeterministicProfileAndProjectSeed(t *testing.T) {
	repository := planRepository(t)
	profileRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileRoot, "AGENTS.md"), []byte("Use Go.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(profileRoot, "AGENTS.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileRoot, ".bashrc"), []byte("alias ll='ls -la'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(profileRoot, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileRoot, ".codex", "mystery.txt"), []byte("DO_NOT_PRINT_UNKNOWN_BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	selectors := []profile.Selector{{Path: "AGENTS.md", Selector: "$"}, {Path: ".bashrc", Selector: "$"}}

	var first bytes.Buffer
	err := RunPlan(context.Background(), repository, profileRoot, selectors, &first)
	if err != nil {
		t.Fatalf("run plan: %v", err)
	}
	var second bytes.Buffer
	if err := RunPlan(context.Background(), repository, profileRoot, selectors, &second); err != nil {
		t.Fatalf("run plan again: %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("plan changed between runs:\n%s\n%s", first.String(), second.String())
	}
	for _, expected := range []string{"project_seed sha256:", "profile_components:\n", "safe:", `component=config:AGENTS.md type=config scope=user trust=declarative path="AGENTS.md" selector="$" selected=true evidence=known_agent_instruction sensitivity=private`, "review:", `component=config:.bashrc type=config scope=user trust=executable path=".bashrc" selector="$" selected=true evidence=known_shell_preferences sensitivity=private`, `source=project_seed path="local.txt" evidence=untracked`, "requires_authorization:", "excluded:", `component=config:.codex/mystery.txt type=config scope=user trust=declarative path=".codex/mystery.txt" selector="$" selected=false evidence=unknown_file_in_known_root sensitivity=unknown`, "conflict:"} {
		if !strings.Contains(first.String(), expected) {
			t.Fatalf("plan lacks %q:\n%s", expected, first.String())
		}
	}
	if strings.Contains(first.String(), "DO_NOT_PRINT_UNKNOWN_BODY") {
		t.Fatal("plan exposed excluded candidate content")
	}
}

func planRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runCommand(t, root, "git", "init", "--bare", remote)
	repository := filepath.Join(root, "work")
	runCommand(t, root, "git", "init", "-b", "main", repository)
	runCommand(t, repository, "git", "config", "user.name", "Test")
	runCommand(t, repository, "git", "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", ".")
	runCommand(t, repository, "git", "commit", "-m", "base")
	runCommand(t, repository, "git", "remote", "add", "origin", "file://"+remote)
	runCommand(t, repository, "git", "push", "-u", "origin", "main")
	if err := os.WriteFile(filepath.Join(repository, "local.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repository
}

func runCommand(t *testing.T, directory, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}
