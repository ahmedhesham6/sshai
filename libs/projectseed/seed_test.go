package projectseed_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/projectseed"
)

func TestPackageCapturesDirtyRepositoryDeterministically(t *testing.T) {
	repository, base := newRepository(t)
	writeFile(t, filepath.Join(repository, "tracked.txt"), "committed\nlocal change\n", 0o644)
	writeFile(t, filepath.Join(repository, "untracked.sh"), "#!/bin/sh\ntouch should-not-exist\n", 0o755)
	writeFile(t, filepath.Join(repository, "ignored.log"), "ignored\n", 0o644)

	first, err := projectseed.Package(context.Background(), repository)
	if err != nil {
		t.Fatalf("package Project Seed: %v", err)
	}
	second, err := projectseed.Package(context.Background(), repository)
	if err != nil {
		t.Fatalf("package Project Seed again: %v", err)
	}

	if first.Digest() != second.Digest() {
		t.Fatalf("Project Seed digest changed: %q != %q", first.Digest(), second.Digest())
	}
	metadata := first.Metadata()
	if metadata.RepositoryURL == "" {
		t.Fatal("repository URL is empty")
	}
	if metadata.BaseRevision != base {
		t.Fatalf("base revision = %q, want %q", metadata.BaseRevision, base)
	}
	if metadata.Revision == base || metadata.BundleDigest == "" {
		t.Fatalf("unpushed commit was not represented: %+v", metadata)
	}
	if metadata.PatchDigest == "" || !strings.Contains(string(first.Patch()), "+local change") {
		t.Fatalf("tracked change was not represented: %q", first.Patch())
	}
	manifest := first.Manifest()
	if len(manifest) != 1 || manifest[0].Path != "untracked.sh" || !manifest[0].Executable {
		t.Fatalf("untracked manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(repository, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("selected executable content ran during inspection or packaging")
	}

	patch := first.Patch()
	patch[0] = 'X'
	if first.Patch()[0] == 'X' {
		t.Fatal("Project Seed patch was mutable")
	}
}

func TestPackageBundlesCompleteHistoryWithoutUpstream(t *testing.T) {
	repository, _ := newRepository(t)
	runGit(t, repository, "branch", "--unset-upstream")
	revision := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))

	seed, err := projectseed.Package(context.Background(), repository)
	if err != nil {
		t.Fatalf("package repository without upstream: %v", err)
	}
	metadata := seed.Metadata()
	if metadata.BaseRevision != revision {
		t.Fatalf("base revision = %q, want local revision %q", metadata.BaseRevision, revision)
	}
	if metadata.BundleDigest == "" || len(seed.Bundle()) == 0 {
		t.Fatal("complete local history was not bundled")
	}
}

func TestPackageRejectsUntrackedSymlink(t *testing.T) {
	repository, _ := newRepository(t)
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(repository, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := projectseed.Package(context.Background(), repository); err == nil {
		t.Fatal("expected untracked symlink to be rejected")
	}
}

func TestPackageRejectsSecretContentBeforePackaging(t *testing.T) {
	repository, _ := newRepository(t)
	writeFile(t, filepath.Join(repository, "credential.pem"), "-----BEGIN PRIVATE KEY-----\nsecret\n", 0o600)
	if _, err := projectseed.Package(context.Background(), repository); err == nil {
		t.Fatal("expected secret-containing untracked content to be rejected")
	}
}

func newRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	run(t, root, "git", "init", "--bare", remote)
	repository := filepath.Join(root, "work")
	run(t, root, "git", "init", "-b", "main", repository)
	runGit(t, repository, "config", "user.name", "Test")
	runGit(t, repository, "config", "user.email", "test@example.com")
	writeFile(t, filepath.Join(repository, "tracked.txt"), "committed\n", 0o644)
	writeFile(t, filepath.Join(repository, ".gitignore"), "*.log\n", 0o644)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "commit", "-m", "base")
	runGit(t, repository, "remote", "add", "origin", remote)
	runGit(t, repository, "push", "-u", "origin", "main")
	base := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
	writeFile(t, filepath.Join(repository, "unpushed.txt"), "unpushed\n", 0o644)
	runGit(t, repository, "add", "unpushed.txt")
	runGit(t, repository, "commit", "-m", "unpushed")
	return repository, base
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	return run(t, directory, "git", args...)
}

func run(t *testing.T, directory, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
