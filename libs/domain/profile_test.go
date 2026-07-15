package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestCreateProfileAndImmutableVersionOwnPublicationState(t *testing.T) {
	createdAt := time.Now()
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: "profile-1", OwnerUserID: "user-1", Name: "Personal", Slug: "personal", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("CreateProfile(): %v", err)
	}
	version, err := profile.PublishVersion(nil, nil, domain.ProfileVersionPublication{
		ID: "version-1", Digest: sha256Digest('a'), CreatedAt: createdAt,
		Artifacts: []domain.ProfileArtifact{{
			ID: "artifact-1", ProfileVersionID: "version-1",
			Kind: domain.ArtifactAgentInstruction, SourceLocator: "AGENTS.md#$", SourceDigest: sha256Digest('b'),
			ContentDigest: sha256Digest('c'), SizeBytes: 42, Mode: 0o640, Sensitivity: domain.SensitivityPrivate,
			Trust: domain.TrustUserAuthored, ContainsExecutable: false,
		}, {
			ID: "artifact-2", ProfileVersionID: "version-1",
			Kind: domain.ArtifactShellPreferences, SourceLocator: ".bashrc#$", SourceDigest: sha256Digest('d'),
			ContentDigest: sha256Digest('e'), Sensitivity: domain.SensitivityPrivate,
			Trust: domain.TrustUserAuthored, ContainsExecutable: true,
		}},
	})
	if err != nil {
		t.Fatalf("PublishProfileVersion(): %v", err)
	}
	if profile.Snapshot().OwnerUserID != "user-1" || version.Snapshot().ProfileID != "profile-1" {
		t.Fatalf("Profile publication = profile:%#v version:%#v", profile.Snapshot(), version.Snapshot())
	}
	artifacts := version.Snapshot().Artifacts
	artifacts[0].SourceLocator = "changed"
	if version.Snapshot().Artifacts[0].SourceLocator != "AGENTS.md#$" {
		t.Fatal("Profile Version artifacts were mutable")
	}
	if !version.Snapshot().Artifacts[1].ContainsExecutable {
		t.Fatal("executable artifact classification was lost")
	}
	if artifacts[0].SizeBytes != 42 || artifacts[0].Mode != 0o640 {
		t.Fatalf("artifact filesystem metadata = %#v", artifacts[0])
	}
}

func TestProfileVersionRejectsUnsafeOrInvalidArtifacts(t *testing.T) {
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: "profile-1", OwnerUserID: "user-1", Name: "Personal", Slug: "personal", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateProfile(): %v", err)
	}
	base := domain.ProfileVersionPublication{
		ID: "version-1", Digest: sha256Digest('a'), CreatedAt: time.Now(),
		Artifacts: []domain.ProfileArtifact{{
			ID: "artifact-1", ProfileVersionID: "version-1",
			Kind: domain.ArtifactAgentInstruction, SourceLocator: "AGENTS.md#$", SourceDigest: sha256Digest('b'),
			ContentDigest: sha256Digest('c'), Sensitivity: domain.SensitivityPrivate, Trust: domain.TrustUserAuthored,
		}},
	}
	for _, invalid := range []struct {
		name   string
		mutate func(*domain.ProfileVersionPublication)
	}{
		{name: "missing artifact", mutate: func(input *domain.ProfileVersionPublication) { input.Artifacts = nil }},
		{name: "credential", mutate: func(input *domain.ProfileVersionPublication) {
			input.Artifacts[0].Sensitivity = domain.SensitivityCredential
		}},
		{name: "unknown sensitivity", mutate: func(input *domain.ProfileVersionPublication) {
			input.Artifacts[0].Sensitivity = domain.SensitivityUnknown
		}},
		{name: "invalid source digest", mutate: func(input *domain.ProfileVersionPublication) { input.Artifacts[0].SourceDigest = "invalid" }},
		{name: "invalid content digest", mutate: func(input *domain.ProfileVersionPublication) { input.Artifacts[0].ContentDigest = "invalid" }},
		{name: "negative size", mutate: func(input *domain.ProfileVersionPublication) { input.Artifacts[0].SizeBytes = -1 }},
		{name: "invalid mode", mutate: func(input *domain.ProfileVersionPublication) { input.Artifacts[0].Mode = 0o1000 }},
		{name: "invalid version digest", mutate: func(input *domain.ProfileVersionPublication) { input.Digest = "invalid" }},
		{name: "wrong Profile Version identity", mutate: func(input *domain.ProfileVersionPublication) { input.Artifacts[0].ProfileVersionID = "other" }},
		{name: "duplicate locator", mutate: func(input *domain.ProfileVersionPublication) {
			duplicate := input.Artifacts[0]
			duplicate.ID = "artifact-2"
			input.Artifacts = append(input.Artifacts, duplicate)
		}},
		{name: "duplicate artifact identity", mutate: func(input *domain.ProfileVersionPublication) {
			duplicate := input.Artifacts[0]
			duplicate.SourceLocator = "CLAUDE.md#$"
			input.Artifacts = append(input.Artifacts, duplicate)
		}},
		{name: "unknown kind", mutate: func(input *domain.ProfileVersionPublication) {
			input.Artifacts[0].Kind = domain.ArtifactKind("unknown")
		}},
		{name: "unknown trust", mutate: func(input *domain.ProfileVersionPublication) {
			input.Artifacts[0].Trust = domain.TrustUnknown
		}},
		{name: "invalid trust", mutate: func(input *domain.ProfileVersionPublication) {
			input.Artifacts[0].Trust = domain.TrustClass("invalid")
		}},
		{name: "false executable classification", mutate: func(input *domain.ProfileVersionPublication) {
			input.Artifacts[0].ContainsExecutable = true
		}},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			input := base
			input.Artifacts = append([]domain.ProfileArtifact(nil), base.Artifacts...)
			invalid.mutate(&input)
			if _, err := profile.PublishVersion(nil, nil, input); err == nil {
				t.Fatal("invalid Profile Version was accepted")
			}
		})
	}
}

func TestProfilePublishesOnlyTheNextVersionFromExpectedCurrentHead(t *testing.T) {
	now := time.Now()
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: "profile-1", OwnerUserID: "user-1", Name: "Personal", Slug: "personal", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	publication := func(id, artifactID string) domain.ProfileVersionPublication {
		return domain.ProfileVersionPublication{
			ID: id, Digest: sha256Digest(id[len(id)-1]), CreatedAt: now,
			Artifacts: []domain.ProfileArtifact{{
				ID: artifactID, ProfileVersionID: id, Kind: domain.ArtifactAgentInstruction,
				SourceLocator: "AGENTS.md#$", SourceDigest: sha256Digest('b'), ContentDigest: sha256Digest('c'),
				Sensitivity: domain.SensitivityPrivate, Trust: domain.TrustUserAuthored,
			}},
		}
	}
	first, err := profile.PublishVersion(nil, nil, publication("version-1", "artifact-1"))
	if err != nil {
		t.Fatalf("publish first Profile Version: %v", err)
	}
	expected := "version-1"
	second, err := profile.PublishVersion(&first, &expected, publication("version-2", "artifact-2"))
	if err != nil {
		t.Fatalf("publish second Profile Version: %v", err)
	}
	secondSnapshot := second.Snapshot()
	if secondSnapshot.Version != 2 || secondSnapshot.ParentVersionID == nil || *secondSnapshot.ParentVersionID != "version-1" {
		t.Fatalf("second Profile Version = %#v", secondSnapshot)
	}
	*secondSnapshot.ParentVersionID = "changed"
	if got := second.Snapshot().ParentVersionID; got == nil || *got != "version-1" {
		t.Fatal("Profile Version parent pointer was mutable")
	}
	stale := "version-stale"
	if _, err := profile.PublishVersion(&first, &stale, publication("version-3", "artifact-3")); !errors.Is(err, domain.ErrStaleProfileHead) {
		t.Fatalf("stale publication error = %v", err)
	}
	foreignProfile, _ := domain.CreateProfile(domain.ProfileSnapshot{
		ID: "profile-2", OwnerUserID: "user-1", Name: "Other", Slug: "other", CreatedAt: now,
	})
	if _, err := foreignProfile.PublishVersion(&first, &expected, publication("version-3", "artifact-3")); err == nil {
		t.Fatal("cross-Profile head was accepted")
	}
	if _, err := profile.PublishVersion(&first, &expected, publication("version-1", "artifact-3")); err == nil {
		t.Fatal("Profile Version reused its head identity")
	}
}

func TestProfileCopiesAndCanonicalizesArchiveState(t *testing.T) {
	createdAt := time.Now()
	archivedAt := createdAt.Add(time.Minute)
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: "profile-1", OwnerUserID: "user-1", Name: "Personal", Slug: "personal",
		CreatedAt: createdAt, ArchivedAt: &archivedAt,
	})
	if err != nil {
		t.Fatalf("CreateProfile(): %v", err)
	}
	snapshot := profile.Snapshot()
	if snapshot.CreatedAt.Location() != time.UTC || snapshot.ArchivedAt == nil || snapshot.ArchivedAt.Location() != time.UTC || snapshot.CreatedAt == createdAt {
		t.Fatalf("Profile timestamps were not canonicalized: %#v", snapshot)
	}
	*snapshot.ArchivedAt = archivedAt.Add(time.Hour)
	if got := profile.Snapshot().ArchivedAt; got == nil || !got.Equal(archivedAt) {
		t.Fatal("Profile archive pointer was mutable")
	}
	validPublication := domain.ProfileVersionPublication{
		ID: "version-1", Digest: sha256Digest('a'), CreatedAt: createdAt,
		Artifacts: []domain.ProfileArtifact{{
			ID: "artifact-1", ProfileVersionID: "version-1", Kind: domain.ArtifactAgentInstruction,
			SourceLocator: "AGENTS.md#$", SourceDigest: sha256Digest('b'), ContentDigest: sha256Digest('c'),
			Sensitivity: domain.SensitivityPrivate, Trust: domain.TrustUserAuthored,
		}},
	}
	if _, err := profile.PublishVersion(nil, nil, validPublication); err == nil {
		t.Fatal("archived Profile accepted publication")
	}
}

func TestRestoreProfileVersionValidatesAndCopiesPersistedHead(t *testing.T) {
	createdAt := time.Now()
	parent := "version-1"
	snapshot := domain.ProfileVersionSnapshot{
		ID: "version-2", ProfileID: "profile-1", ParentVersionID: &parent,
		Version: 2, Digest: sha256Digest('a'), CreatedAt: createdAt,
		Artifacts: []domain.ProfileArtifact{{
			ID: "artifact-2", ProfileVersionID: "version-2", Kind: domain.ArtifactAgentInstruction,
			SourceLocator: "AGENTS.md#$", SourceDigest: sha256Digest('b'), ContentDigest: sha256Digest('c'),
			Sensitivity: domain.SensitivityPrivate, Trust: domain.TrustUserAuthored,
		}},
	}
	version, err := domain.RestoreProfileVersion(snapshot)
	if err != nil {
		t.Fatalf("RestoreProfileVersion(): %v", err)
	}
	parent = "changed"
	snapshot.Artifacts[0].SourceLocator = "changed"
	restored := version.Snapshot()
	if restored.ParentVersionID == nil || *restored.ParentVersionID != "version-1" || restored.Artifacts[0].SourceLocator != "AGENTS.md#$" {
		t.Fatalf("restored Profile Version was mutable: %#v", restored)
	}
	if restored.CreatedAt.Location() != time.UTC || !restored.CreatedAt.Equal(createdAt) || restored.CreatedAt == createdAt {
		t.Fatalf("restored creation time was not canonicalized: input=%s output=%s", createdAt, restored.CreatedAt)
	}
	self := snapshot
	self.ParentVersionID = &self.ID
	if _, err := domain.RestoreProfileVersion(self); err == nil {
		t.Fatal("self-parented persisted Profile Version was accepted")
	}
}
