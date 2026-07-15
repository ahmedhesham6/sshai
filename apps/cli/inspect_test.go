package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInspectReportsContentFreeLocalEvidenceDeterministically(t *testing.T) {
	repository := planRepository(t)
	runCommand(t, repository, "git", "remote", "set-url", "origin", "https://user:ORIGIN_SECRET@example.com/org/repo.git")
	profileRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileRoot, "AGENTS.md"), []byte("DO_NOT_PRINT_PROFILE_BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(profileRoot, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileRoot, ".codex", "unknown.txt"), []byte("DO_NOT_PRINT_UNKNOWN_BODY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sshDirectory := t.TempDir()
	publicKey, fingerprint := writeEd25519KeyPair(t, sshDirectory, "id_work", "private comment")

	var first bytes.Buffer
	if err := RunInspect(context.Background(), repository, profileRoot, sshDirectory, &first); err != nil {
		t.Fatalf("RunInspect(): %v", err)
	}
	var second bytes.Buffer
	if err := RunInspect(context.Background(), repository, profileRoot, sshDirectory, &second); err != nil {
		t.Fatalf("RunInspect() replay: %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("inspection changed between runs:\n%s\n%s", first.String(), second.String())
	}
	for _, expected := range []string{
		"project:\n",
		"  revision=", "  base_revision=", "  untracked path=\"local.txt\"\n",
		"profile_candidates:\n",
		`source=profile type=agent_instruction path="AGENTS.md"`,
		`source=profile type=unknown path=".codex/unknown.txt"`,
		"ssh_keys:\n",
		`label="id_work"`, "fingerprint=" + fingerprint,
		`private_key_path="` + filepath.Join(sshDirectory, "id_work") + `"`,
	} {
		if !strings.Contains(first.String(), expected) {
			t.Fatalf("inspection lacks %q:\n%s", expected, first.String())
		}
	}
	for _, forbidden := range []string{publicKey, "private comment", "private key contents", "DO_NOT_PRINT_PROFILE_BODY", "DO_NOT_PRINT_UNKNOWN_BODY", "ORIGIN_SECRET", "example.com/org/repo.git"} {
		if strings.Contains(first.String(), forbidden) {
			t.Fatalf("inspection exposed %q", forbidden)
		}
	}
}

func TestCLIInspectUsesRepositoryProfileAndSSHRoots(t *testing.T) {
	repository := planRepository(t)
	profileRoot := t.TempDir()
	sshDirectory := t.TempDir()
	writeEd25519KeyPair(t, sshDirectory, "id_devm", "")
	var output bytes.Buffer
	application := cli{
		output:           &output,
		workingDirectory: func() (string, error) { return repository, nil },
		sshDirectory:     func() (string, error) { return sshDirectory, nil },
	}
	if err := application.run(context.Background(), []string{"inspect", "--profile-root", profileRoot}); err != nil {
		t.Fatalf("route inspect: %v", err)
	}
	if !strings.Contains(output.String(), `label="id_devm"`) {
		t.Fatalf("inspect output = %s", output.String())
	}
}
