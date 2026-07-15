package domain_test

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestRegisterProjectSeedOwnsImmutableContentAddressedMetadata(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	seed, err := domain.RegisterProjectSeed(domain.ProjectSeedSnapshot{
		ID: "seed-1", OwnerUserID: "user-1", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: sha256Digest('a'), GitBundleDigest: sha256Digest('b'),
		TrackedPatchDigest: sha256Digest('c'), UntrackedBundleDigest: sha256Digest('d'),
		ManifestDigest: sha256Digest('e'), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("RegisterProjectSeed(): %v", err)
	}
	snapshot := seed.Snapshot()
	if snapshot.ID != "seed-1" || snapshot.OwnerUserID != "user-1" || snapshot.Digest != sha256Digest('a') {
		t.Fatalf("Project Seed snapshot = %#v", snapshot)
	}
}

func TestRegisterProjectSeedRejectsIncompleteOrInvalidMetadata(t *testing.T) {
	base := domain.ProjectSeedSnapshot{
		ID: "seed-1", OwnerUserID: "user-1", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: sha256Digest('a'), ManifestDigest: sha256Digest('b'), CreatedAt: time.Now(),
	}
	for name, mutate := range map[string]func(*domain.ProjectSeedSnapshot){
		"ID":       func(input *domain.ProjectSeedSnapshot) { input.ID = "  " },
		"owner":    func(input *domain.ProjectSeedSnapshot) { input.OwnerUserID = "\t" },
		"URL":      func(input *domain.ProjectSeedSnapshot) { input.RepositoryURL = "not a URL" },
		"revision": func(input *domain.ProjectSeedSnapshot) { input.BaseRevision = " " },
		"digest":   func(input *domain.ProjectSeedSnapshot) { input.Digest = "sha256:nope" },
		"manifest": func(input *domain.ProjectSeedSnapshot) { input.ManifestDigest = "" },
		"time":     func(input *domain.ProjectSeedSnapshot) { input.CreatedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := domain.RegisterProjectSeed(input); err == nil {
				t.Fatal("invalid Project Seed was accepted")
			}
		})
	}
}

func TestRegisterProjectSeedRejectsUntrustedRepositoryURLsAndOptionalDigests(t *testing.T) {
	base := domain.ProjectSeedSnapshot{
		ID: "seed-1", OwnerUserID: "user-1", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: sha256Digest('a'), ManifestDigest: sha256Digest('b'), CreatedAt: time.Now(),
	}
	for _, repositoryURL := range []string{
		"http://github.com/example/project.git",
		"file:///tmp/project",
		"https://token@github.com/example/project.git",
		"https://github.com/example/project.git?token=secret",
		"https://github.com/example/project.git#secret",
	} {
		input := base
		input.RepositoryURL = repositoryURL
		if _, err := domain.RegisterProjectSeed(input); err == nil {
			t.Fatalf("repository URL %q was accepted", repositoryURL)
		}
	}
	for name, mutate := range map[string]func(*domain.ProjectSeedSnapshot){
		"Git bundle":       func(input *domain.ProjectSeedSnapshot) { input.GitBundleDigest = "invalid" },
		"tracked patch":    func(input *domain.ProjectSeedSnapshot) { input.TrackedPatchDigest = "invalid" },
		"untracked bundle": func(input *domain.ProjectSeedSnapshot) { input.UntrackedBundleDigest = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := domain.RegisterProjectSeed(input); err == nil {
				t.Fatal("invalid optional digest was accepted")
			}
		})
	}
}

func TestProjectSeedSnapshotIsCopiedAndCanonicalizesCreationTime(t *testing.T) {
	createdAt := time.Now()
	seed, err := domain.RegisterProjectSeed(domain.ProjectSeedSnapshot{
		ID: "seed-1", OwnerUserID: "user-1", RepositoryURL: "ssh://github.com/example/project.git",
		BaseRevision: "abc123", Digest: sha256Digest('a'), ManifestDigest: sha256Digest('b'), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("RegisterProjectSeed(): %v", err)
	}
	snapshot := seed.Snapshot()
	if snapshot.CreatedAt.Location() != time.UTC || !snapshot.CreatedAt.Equal(createdAt) || snapshot.CreatedAt == createdAt {
		t.Fatalf("creation time was not canonicalized: input=%s output=%s", createdAt, snapshot.CreatedAt)
	}
	snapshot.RepositoryURL = "https://changed.example/project.git"
	if seed.Snapshot().RepositoryURL != "ssh://github.com/example/project.git" {
		t.Fatal("Project Seed snapshot was mutable")
	}
}

func sha256Digest(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return "sha256:" + string(value)
}
