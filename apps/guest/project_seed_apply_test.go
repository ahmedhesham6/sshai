package guest_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/projectseed"
)

func TestProjectSeedApplicationChecksOutTheExactBaseAndReplaysSafely(t *testing.T) {
	repository, base := seedRepository(t)
	application, err := guest.NewProjectSeedApplication(guest.ProjectSeedApplicationInput{
		RepositoryURL: repositoryURL(repository),
		BaseRevision:  base,
		Manifest:      seedArtifact([]byte("[]")),
	})
	if err != nil {
		t.Fatalf("create Project Seed application: %v", err)
	}

	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := application.Apply(context.Background(), workspace); err != nil {
		t.Fatalf("apply Project Seed: %v", err)
	}
	if head := strings.TrimSpace(seedRunGit(t, workspace, "rev-parse", "HEAD")); head != base {
		t.Fatalf("workspace HEAD = %q, want exact base %q", head, base)
	}

	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("remote authority\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := application.Apply(context.Background(), workspace); err != nil {
		t.Fatalf("replay Project Seed: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(workspace, "tracked.txt")); err != nil || string(content) != "remote authority\n" {
		t.Fatalf("replay changed authoritative workspace content = %q, %v", content, err)
	}
}

func TestProjectSeedApplicationRestoresBundledDirtyRepositoryState(t *testing.T) {
	remote, _ := seedRepository(t)
	source := filepath.Join(t.TempDir(), "source")
	seedRun(t, filepath.Dir(source), "git", "clone", repositoryURL(remote), source)
	seedRunGit(t, source, "config", "user.name", "Test")
	seedRunGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "unpushed.txt"), []byte("local commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunGit(t, source, "add", "unpushed.txt")
	seedRunGit(t, source, "commit", "-m", "unpushed")
	localRevision := strings.TrimSpace(seedRunGit(t, source, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("dirty tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "local.sh"), []byte("#!/bin/sh\necho inert\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "local.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed, err := projectseed.Package(context.Background(), source)
	if err != nil {
		t.Fatalf("package Project Seed fixture: %v", err)
	}
	manifest, err := json.Marshal(seed.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	application, err := guest.NewProjectSeedApplication(guest.ProjectSeedApplicationInput{
		RepositoryURL: seed.Metadata().RepositoryURL,
		BaseRevision:  seed.Metadata().BaseRevision,
		GitBundle:     seedArtifact(seed.Bundle()),
		TrackedPatch:  seedArtifact(seed.Patch()),
		UntrackedTar:  seedArtifact(seed.Archive()),
		Manifest:      seedArtifact(manifest),
	})
	if err != nil {
		t.Fatalf("create Project Seed application: %v", err)
	}

	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := application.Apply(context.Background(), workspace); err != nil {
		t.Fatalf("apply dirty Project Seed: %v", err)
	}
	if head := strings.TrimSpace(seedRunGit(t, workspace, "rev-parse", "HEAD")); head != localRevision {
		t.Fatalf("workspace HEAD = %q, want bundled revision %q", head, localRevision)
	}
	if content, err := os.ReadFile(filepath.Join(workspace, "tracked.txt")); err != nil || string(content) != "dirty tracked\n" {
		t.Fatalf("tracked change = %q, %v", content, err)
	}
	info, err := os.Stat(filepath.Join(workspace, "local.sh"))
	if err != nil {
		t.Fatalf("stat untracked file: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("untracked file mode = %o, want 755", info.Mode().Perm())
	}
}

func TestProjectSeedApplicationImportsNamedBundleRefsWithoutChangingTheBundledHead(t *testing.T) {
	remote, base := seedRepository(t)
	source := filepath.Join(t.TempDir(), "source")
	seedRun(t, filepath.Dir(source), "git", "clone", repositoryURL(remote), source)
	seedRunGit(t, source, "config", "user.name", "Test")
	seedRunGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "main.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunGit(t, source, "add", "main.txt")
	seedRunGit(t, source, "commit", "-m", "main")
	bundledHead := strings.TrimSpace(seedRunGit(t, source, "rev-parse", "HEAD"))
	seedRunGit(t, source, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunGit(t, source, "add", "feature.txt")
	seedRunGit(t, source, "commit", "-m", "feature")
	feature := strings.TrimSpace(seedRunGit(t, source, "rev-parse", "HEAD"))
	seedRunGit(t, source, "switch", "main")
	bundlePath := filepath.Join(t.TempDir(), "seed.bundle")
	seedRunGit(t, source, "bundle", "create", bundlePath, "HEAD", "refs/heads/feature", "^"+base)
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	application, err := guest.NewProjectSeedApplication(guest.ProjectSeedApplicationInput{
		RepositoryURL: repositoryURL(remote),
		BaseRevision:  base,
		GitBundle:     seedArtifact(bundle),
		Manifest:      seedArtifact([]byte("[]")),
	})
	if err != nil {
		t.Fatalf("create Project Seed application: %v", err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := application.Apply(context.Background(), workspace); err != nil {
		t.Fatalf("apply Project Seed: %v", err)
	}
	if head := strings.TrimSpace(seedRunGit(t, workspace, "rev-parse", "HEAD")); head != bundledHead {
		t.Fatalf("workspace HEAD = %q, want bundle HEAD %q", head, bundledHead)
	}
	if imported := strings.TrimSpace(seedRunGit(t, workspace, "rev-parse", "refs/sshai/project-seed/heads/feature")); imported != feature {
		t.Fatalf("imported feature = %q, want %q", imported, feature)
	}
}

func TestProjectSeedApplicationRejectsANonArrayManifest(t *testing.T) {
	remote, base := seedRepository(t)
	if _, err := guest.NewProjectSeedApplication(guest.ProjectSeedApplicationInput{
		RepositoryURL: repositoryURL(remote),
		BaseRevision:  base,
		Manifest:      seedArtifact([]byte("null")),
	}); err == nil {
		t.Fatal("non-array Project Seed manifest was accepted")
	}
}

func TestProjectSeedApplicationRejectsUnverifiedAndUnsafeArtifacts(t *testing.T) {
	remote, base := seedRepository(t)
	emptyDigest := seedArtifact(nil).SHA256
	regular := testManifestEntry{Path: "local.txt", Mode: 0o644, Size: 4, ContentDigest: seedArtifact([]byte("safe")).SHA256}
	symlink := regular
	symlink.Size = 0
	symlink.Mode = 0o777
	symlink.Executable = true
	symlink.ContentDigest = emptyDigest

	for _, test := range []struct {
		name     string
		manifest guest.ProjectSeedArtifact
		archive  guest.ProjectSeedArtifact
		patch    guest.ProjectSeedArtifact
	}{
		{
			name:     "artifact digest mismatch",
			manifest: seedArtifact([]byte("[]")),
			patch: guest.ProjectSeedArtifact{
				SHA256: "sha256:" + strings.Repeat("0", 64), Content: []byte("unverified"),
			},
		},
		{
			name:     "traversal manifest path",
			manifest: manifestArtifact(t, []testManifestEntry{{Path: "../escape", Mode: 0o644, ContentDigest: emptyDigest}}),
		},
		{
			name:     "symlink archive entry",
			manifest: manifestArtifact(t, []testManifestEntry{symlink}),
			archive:  tarArtifact(t, &tar.Header{Name: "local.txt", Typeflag: tar.TypeSymlink, Linkname: "../escape", Mode: 0o777}, nil),
		},
		{
			name:     "content disagrees with manifest",
			manifest: manifestArtifact(t, []testManifestEntry{regular}),
			archive:  tarArtifact(t, &tar.Header{Name: "local.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}, []byte("evil")),
		},
		{
			name:     "undeclared archive path",
			manifest: seedArtifact([]byte("[]")),
			archive:  tarArtifact(t, &tar.Header{Name: "escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}, []byte("x")),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := guest.NewProjectSeedApplication(guest.ProjectSeedApplicationInput{
				RepositoryURL: repositoryURL(remote), BaseRevision: base,
				TrackedPatch: test.patch, UntrackedTar: test.archive, Manifest: test.manifest,
			}); err == nil {
				t.Fatal("unsafe Project Seed was accepted")
			}
		})
	}
}

func TestProjectSeedApplicationRejectsAnExistingDivergentWorkspace(t *testing.T) {
	remote, base := seedRepository(t)
	application, err := guest.NewProjectSeedApplication(guest.ProjectSeedApplicationInput{
		RepositoryURL: repositoryURL(remote), BaseRevision: base, Manifest: seedArtifact([]byte("[]")),
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	divergent := filepath.Join(workspace, "keep.txt")
	if err := os.WriteFile(divergent, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := application.Apply(context.Background(), workspace); err == nil {
		t.Fatal("divergent workspace was overwritten")
	}
	if content, err := os.ReadFile(divergent); err != nil || string(content) != "keep\n" {
		t.Fatalf("divergent workspace content = %q, %v", content, err)
	}
}

func TestProjectSeedApplicationOwnsAnImmutableInputCopy(t *testing.T) {
	remote, base := seedRepository(t)
	manifest := []byte("[]")
	input := guest.ProjectSeedApplicationInput{
		RepositoryURL: repositoryURL(remote), BaseRevision: base, Manifest: seedArtifact(manifest),
	}
	application, err := guest.NewProjectSeedApplication(input)
	if err != nil {
		t.Fatal(err)
	}
	manifest[0] = '{'
	input.RepositoryURL = "file:///changed"
	input.Manifest.Content[0] = 'x'
	if err := application.Apply(context.Background(), filepath.Join(t.TempDir(), "workspace")); err != nil {
		t.Fatalf("apply Project Seed after caller mutated its input: %v", err)
	}
}

func TestProjectSeedApplicationIgnoresAmbientGitExecutionConfiguration(t *testing.T) {
	remote, _ := seedRepository(t)
	source := filepath.Join(t.TempDir(), "source")
	seedRun(t, filepath.Dir(source), "git", "clone", repositoryURL(remote), source)
	seedRunGit(t, source, "config", "user.name", "Test")
	seedRunGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, ".gitattributes"), []byte("tracked.txt filter=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunGit(t, source, "add", ".gitattributes")
	seedRunGit(t, source, "commit", "-m", "attributes")
	seedRunGit(t, source, "push", "origin", "main")
	base := strings.TrimSpace(seedRunGit(t, source, "rev-parse", "HEAD"))

	outside := t.TempDir()
	template := filepath.Join(outside, "template")
	if err := os.MkdirAll(filepath.Join(template, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	hookMarker := filepath.Join(outside, "hook-ran")
	hook := filepath.Join(template, "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+hookMarker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	filterMarker := filepath.Join(outside, "filter-ran")
	filter := filepath.Join(outside, "filter.sh")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\ntouch "+filterMarker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filter, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("GIT_CONFIG_KEY_0", "init.templateDir")
	t.Setenv("GIT_CONFIG_VALUE_0", template)
	t.Setenv("GIT_CONFIG_KEY_1", "filter.evil.smudge")
	t.Setenv("GIT_CONFIG_VALUE_1", filter)
	t.Setenv("GIT_CONFIG_KEY_2", "filter.evil.required")
	t.Setenv("GIT_CONFIG_VALUE_2", "true")

	application, err := guest.NewProjectSeedApplication(guest.ProjectSeedApplicationInput{
		RepositoryURL: repositoryURL(remote), BaseRevision: base, Manifest: seedArtifact([]byte("[]")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Apply(context.Background(), filepath.Join(t.TempDir(), "workspace")); err != nil {
		t.Fatalf("apply Project Seed with hostile ambient Git config: %v", err)
	}
	for _, marker := range []string{hookMarker, filterMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("ambient Git configuration executed %q", marker)
		}
	}
}

type testManifestEntry struct {
	Path          string
	Mode          uint32
	Size          int64
	Executable    bool
	ContentDigest string
}

func manifestArtifact(t *testing.T, entries []testManifestEntry) guest.ProjectSeedArtifact {
	t.Helper()
	content, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return seedArtifact(content)
}

func tarArtifact(t *testing.T, header *tar.Header, content []byte) guest.ProjectSeedArtifact {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return seedArtifact(archive.Bytes())
}

func seedRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seedRun(t, root, "git", "init", "--bare", remote)
	repository := filepath.Join(root, "source")
	seedRun(t, root, "git", "init", "-b", "main", repository)
	seedRunGit(t, repository, "config", "user.name", "Test")
	seedRunGit(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunGit(t, repository, "add", "tracked.txt")
	seedRunGit(t, repository, "commit", "-m", "base")
	seedRunGit(t, repository, "remote", "add", "origin", repositoryURL(remote))
	seedRunGit(t, repository, "push", "-u", "origin", "main")
	base := strings.TrimSpace(seedRunGit(t, repository, "rev-parse", "HEAD"))
	return remote, base
}

func repositoryURL(path string) string {
	return "file://" + filepath.ToSlash(path)
}

func seedArtifact(content []byte) guest.ProjectSeedArtifact {
	sum := sha256.Sum256(content)
	return guest.ProjectSeedArtifact{SHA256: "sha256:" + hex.EncodeToString(sum[:]), Content: content}
}

func seedRunGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	return seedRun(t, directory, "git", arguments...)
}

func seedRun(t *testing.T, directory, name string, arguments ...string) string {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, arguments, err, output)
	}
	return string(output)
}
