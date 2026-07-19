package domain_test

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestCreateProfileAndImmutableVersionOwnCapsuleRefState(t *testing.T) {
	createdAt := time.Now()
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: "profile-1", OwnerUserID: "user-1", Name: "Personal", Slug: "personal", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("CreateProfile(): %v", err)
	}
	version, err := profile.PublishVersion(nil, nil, domain.ProfileVersionPublication{
		ID: "version-1", Digest: sha256Digest('a'), CreatedAt: createdAt,
		CapsuleRefs: []domain.CapsuleRef{
			{Ref: "registry.example.com/team/base:stable", FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"config:editor"}},
			{Ref: "registry.example.com/team/tools@sha256:" + repeatedHex('b'), FreshnessPolicy: domain.FreshnessPin},
		},
	})
	if err != nil {
		t.Fatalf("PublishProfileVersion(): %v", err)
	}
	if profile.Snapshot().OwnerUserID != "user-1" || version.Snapshot().ProfileID != "profile-1" {
		t.Fatalf("Profile publication = profile:%#v version:%#v", profile.Snapshot(), version.Snapshot())
	}
	refs := version.Snapshot().CapsuleRefs
	refs[0].Ref = "changed"
	refs[0].Exclusions[0] = "changed"
	if got := version.Snapshot().CapsuleRefs; got[0].Ref != "registry.example.com/team/base:stable" || got[0].Exclusions[0] != "config:editor" {
		t.Fatal("Profile Version Capsule Refs were mutable")
	}
}

func TestProfileVersionRejectsInvalidCapsuleRefs(t *testing.T) {
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: "profile-1", OwnerUserID: "user-1", Name: "Personal", Slug: "personal", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateProfile(): %v", err)
	}
	base := domain.ProfileVersionPublication{
		ID: "version-1", Digest: sha256Digest('a'), CreatedAt: time.Now(),
		CapsuleRefs: []domain.CapsuleRef{{Ref: "registry.example.com/team/base:stable", FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"config:editor"}}},
	}
	for _, invalid := range []struct {
		name   string
		mutate func(*domain.ProfileVersionPublication)
	}{
		{name: "empty Ref", mutate: func(input *domain.ProfileVersionPublication) { input.CapsuleRefs[0].Ref = " " }},
		{name: "malformed Ref", mutate: func(input *domain.ProfileVersionPublication) { input.CapsuleRefs[0].Ref = "not a registry reference" }},
		{name: "invalid freshness policy", mutate: func(input *domain.ProfileVersionPublication) {
			input.CapsuleRefs[0].FreshnessPolicy = domain.FreshnessPolicy("archive")
		}},
		{name: "empty exclusion", mutate: func(input *domain.ProfileVersionPublication) { input.CapsuleRefs[0].Exclusions = []string{" "} }},
		{name: "invalid Profile Version digest", mutate: func(input *domain.ProfileVersionPublication) { input.Digest = "invalid" }},
		{name: "duplicate Ref", mutate: func(input *domain.ProfileVersionPublication) {
			input.CapsuleRefs = append(input.CapsuleRefs, domain.CapsuleRef{Ref: input.CapsuleRefs[0].Ref, FreshnessPolicy: domain.FreshnessPin})
		}},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			input := clonePublication(base)
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
	publication := func(id, ref string) domain.ProfileVersionPublication {
		return domain.ProfileVersionPublication{
			ID: id, Digest: sha256Digest(id[len(id)-1]), CreatedAt: now,
			CapsuleRefs: []domain.CapsuleRef{{Ref: ref, FreshnessPolicy: domain.FreshnessTrack}},
		}
	}
	first, err := profile.PublishVersion(nil, nil, publication("version-1", "registry.example.com/team/base:stable"))
	if err != nil {
		t.Fatalf("publish first Profile Version: %v", err)
	}
	expected := "version-1"
	second, err := profile.PublishVersion(&first, &expected, publication("version-2", "registry.example.com/team/tools:stable"))
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
	if _, err := profile.PublishVersion(&first, &stale, publication("version-3", "registry.example.com/team/other:stable")); !errors.Is(err, domain.ErrStaleProfileHead) {
		t.Fatalf("stale publication error = %v", err)
	}
	if _, err := profile.PublishVersion(&first, nil, publication("version-3", "registry.example.com/team/other:stable")); !errors.Is(err, domain.ErrStaleProfileHead) {
		t.Fatalf("missing expected head error = %v", err)
	}
	foreignProfile, _ := domain.CreateProfile(domain.ProfileSnapshot{
		ID: "profile-2", OwnerUserID: "user-1", Name: "Other", Slug: "other", CreatedAt: now,
	})
	if _, err := foreignProfile.PublishVersion(&first, &expected, publication("version-3", "registry.example.com/team/other:stable")); err == nil {
		t.Fatal("cross-Profile head was accepted")
	}
	if _, err := profile.PublishVersion(&first, &expected, publication("version-1", "registry.example.com/team/other:stable")); err == nil {
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
		CapsuleRefs: []domain.CapsuleRef{{Ref: "registry.example.com/team/base:stable", FreshnessPolicy: domain.FreshnessTrack}},
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
		CapsuleRefs: []domain.CapsuleRef{{Ref: "registry.example.com/team/base:stable", FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"config:editor"}}},
	}
	version, err := domain.RestoreProfileVersion(snapshot)
	if err != nil {
		t.Fatalf("RestoreProfileVersion(): %v", err)
	}
	parent = "changed"
	snapshot.CapsuleRefs[0].Exclusions[0] = "changed"
	restored := version.Snapshot()
	if restored.ParentVersionID == nil || *restored.ParentVersionID != "version-1" || restored.CapsuleRefs[0].Exclusions[0] != "config:editor" {
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

func TestComputeProfileVersionDigestIsOrderedDeterministicAndContentAddressed(t *testing.T) {
	refs := []domain.CapsuleRef{
		{Ref: "registry.example.com/team/base:stable", FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"skill:debug", "config:editor"}},
		{Ref: "registry.example.com/team/tools:stable", FreshnessPolicy: domain.FreshnessPin},
	}
	first := domain.ComputeProfileVersionDigest(refs)
	second := domain.ComputeProfileVersionDigest(append([]domain.CapsuleRef(nil), refs...))
	if first != second {
		t.Fatalf("same ordered Capsule Refs produced different digests: %q != %q", first, second)
	}
	reversed := append([]domain.CapsuleRef(nil), refs...)
	reversed[0], reversed[1] = reversed[1], reversed[0]
	if first == domain.ComputeProfileVersionDigest(reversed) {
		t.Fatal("different Capsule Ref order produced the same Profile Version digest")
	}
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(first) {
		t.Fatalf("Profile Version digest = %q, want a sha256 digest", first)
	}
}

func clonePublication(publication domain.ProfileVersionPublication) domain.ProfileVersionPublication {
	publication.CapsuleRefs = append([]domain.CapsuleRef(nil), publication.CapsuleRefs...)
	for index := range publication.CapsuleRefs {
		publication.CapsuleRefs[index].Exclusions = append([]string(nil), publication.CapsuleRefs[index].Exclusions...)
	}
	return publication
}

func repeatedHex(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return string(value)
}
