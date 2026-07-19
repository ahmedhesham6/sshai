package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

func TestCanonicalRepositoryIdentityUsesNormalizedOriginOrResolvedRoot(t *testing.T) {
	root := t.TempDir()
	git := func(_ context.Context, _ string, arguments ...string) (string, error) {
		switch arguments[0] {
		case "rev-parse":
			return root, nil
		case "remote":
			return "Git@GitHub.COM:Owner/Repo.git/", nil
		default:
			return "", errors.New("unexpected git call")
		}
	}
	identity, gotRoot, err := canonicalRepositoryIdentity(context.Background(), root, git)
	if err != nil {
		t.Fatal(err)
	}
	if identity != "git://github.com/Owner/Repo" || gotRoot != root {
		t.Fatalf("canonical repository = identity:%q root:%q", identity, gotRoot)
	}

	withoutRemote := func(_ context.Context, _ string, arguments ...string) (string, error) {
		if arguments[0] == "rev-parse" {
			return root, nil
		}
		return "", errors.New("origin missing")
	}
	identity, _, err = canonicalRepositoryIdentity(context.Background(), root, withoutRemote)
	if err != nil || identity != "file://"+filepath.ToSlash(root) {
		t.Fatalf("root fallback = %q, %v", identity, err)
	}
	localRemote := filepath.Join(root, "origin.git")
	if err := os.Mkdir(localRemote, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err = normalizeGitRemoteAt("./origin.git", root)
	if err != nil || identity != "file://"+filepath.ToSlash(localRemote) {
		t.Fatalf("relative local origin = %q, %v", identity, err)
	}
}

func TestLocalStateStorePrivatePermissionsConcurrentUpdatesAndBindings(t *testing.T) {
	configDirectory := filepath.Join(t.TempDir(), ".config", "devm")
	store := newLocalStateStore(configDirectory)
	const updates = 12
	var group sync.WaitGroup
	errorsByUpdate := make(chan error, updates)
	for index := range updates {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsByUpdate <- store.UpdateConfig(context.Background(), func(config *localConfig) error {
				config.SSHKeyIDs = append(config.SSHKeyIDs, string(rune('a'+index)))
				return nil
			})
		}()
	}
	group.Wait()
	close(errorsByUpdate)
	for err := range errorsByUpdate {
		if err != nil {
			t.Fatal(err)
		}
	}
	config, err := store.ReadConfig()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(config.SSHKeyIDs)
	if len(config.SSHKeyIDs) != updates || config.SSHKeyIDs[0] != "a" || config.SSHKeyIDs[updates-1] != "l" {
		t.Fatalf("concurrent config = %#v", config.SSHKeyIDs)
	}
	assertMode(t, configDirectory, 0o700)
	assertMode(t, filepath.Join(configDirectory, "config.toml"), 0o600)

	identity := "git://example.test/owner/repository"
	if err := store.BindProject(context.Background(), identity, "env_01"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindProject(context.Background(), identity, "env_01"); err != nil {
		t.Fatalf("idempotent binding: %v", err)
	}
	if err := store.BindProject(context.Background(), identity, "env_other"); !errors.Is(err, errLocalStateConflict) {
		t.Fatalf("conflicting binding = %v", err)
	}
	binding, found, err := store.ReadProject(identity)
	if err != nil || !found || binding.EnvironmentID != "env_01" {
		t.Fatalf("binding = %#v found:%t error:%v", binding, found, err)
	}
	projects := filepath.Join(configDirectory, "projects")
	assertMode(t, projects, 0o700)
	assertMode(t, filepath.Join(projects, projectBindingName(identity)), 0o600)
}

func TestLocalStateStoreRejectsSymlinksAndUnsafeModes(t *testing.T) {
	t.Run("symlink root", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		link := filepath.Join(root, "devm")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := newLocalStateStore(link).UpdateConfig(context.Background(), func(*localConfig) error { return nil }); err == nil {
			t.Fatal("symlinked local state was accepted")
		}
	})
	t.Run("symlink projects", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "projects")); err != nil {
			t.Fatal(err)
		}
		if err := newLocalStateStore(root).BindProject(context.Background(), "git://example.test/a/b", "env_01"); err == nil {
			t.Fatal("symlinked projects directory was accepted")
		}
	})
	t.Run("open config", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\ndefault_region = \"eu-central-1\"\nruntime_preset = \"cpu2-mem8\"\nauto_stop_mode = \"manual\"\nauto_stop_grace_period_seconds = 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := newLocalStateStore(root).ReadConfig(); err == nil {
			t.Fatal("open config permissions were accepted")
		}
	})
}
