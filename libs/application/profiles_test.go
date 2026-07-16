package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestProfileServiceOwnsProfileAndCapsulePublicationIdentity(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &profileRepositoryFake{}
	service := application.NewProfileService(repository, nil, &idsFake{values: []string{"profile-1", "version-1"}}, func() time.Time { return now })

	profile, err := service.CreateProfile(t.Context(), application.CreateProfileInput{
		OwnerUserID: "user-1", Name: "Personal Config", IdempotencyKey: "profile-create-key",
	})
	if err != nil {
		t.Fatalf("CreateProfile(): %v", err)
	}
	if got := profile.Snapshot(); got.ID != "profile-1" || got.Slug != "personal-config" || got.OwnerUserID != "user-1" {
		t.Fatalf("created Profile = %#v", got)
	}

	version, err := service.PublishProfileVersion(t.Context(), application.PublishProfileVersionInput{
		OwnerUserID: "user-1", ProfileID: "profile-1", IdempotencyKey: "profile-publish-key",
		CapsuleRefs: []domain.CapsuleRef{{
			Ref: "registry.example.com/team/base:stable", FreshnessPolicy: domain.FreshnessTrack,
		}},
	})
	if err != nil {
		t.Fatalf("PublishProfileVersion(): %v", err)
	}
	if got := version.Snapshot(); got.ID != "version-1" || len(got.CapsuleRefs) != 1 || got.CapsuleRefs[0].Ref != "registry.example.com/team/base:stable" {
		t.Fatalf("published Profile Version = %#v", got)
	}
	if repository.expectedHead != nil || repository.profileID != "profile-1" || repository.ownerID != "user-1" {
		t.Fatalf("publication repository input = owner:%q Profile:%q head:%v", repository.ownerID, repository.profileID, repository.expectedHead)
	}
}

func TestProfileServiceRejectsUnsupportedForkAndMissingIdempotency(t *testing.T) {
	repository := &profileRepositoryFake{}
	service := application.NewProfileService(repository, nil, &idsFake{values: []string{"unused"}}, time.Now)
	fork := "version-1"
	if _, err := service.CreateProfile(t.Context(), application.CreateProfileInput{
		OwnerUserID: "user-1", Name: "Fork", ForkedFromVersionID: &fork, IdempotencyKey: "profile-create-key",
	}); !errors.Is(err, application.ErrProfileForkUnsupported) {
		t.Fatalf("fork error = %v", err)
	}
	if _, err := service.PublishProfileVersion(t.Context(), application.PublishProfileVersionInput{
		OwnerUserID: "user-1", ProfileID: "profile-1", IdempotencyKey: " ",
	}); !errors.Is(err, application.ErrInvalidProfileCommand) {
		t.Fatalf("missing idempotency error = %v", err)
	}
	if repository.createCalls != 0 || repository.publishCalls != 0 {
		t.Fatalf("invalid commands reached persistence: create=%d publish=%d", repository.createCalls, repository.publishCalls)
	}
}

func TestProfileServiceComputesProfileVersionDigestFromCapsuleRefs(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &profileRepositoryFake{}
	service := application.NewProfileService(repository, nil, &idsFake{values: []string{"version-1"}}, func() time.Time { return now })
	refs := []domain.CapsuleRef{{
		Ref: "registry.example.com/team/base:stable", FreshnessPolicy: domain.FreshnessTrack, Exclusions: []string{"config:editor"},
	}}

	version, err := service.PublishProfileVersion(t.Context(), application.PublishProfileVersionInput{
		OwnerUserID: "user-1", ProfileID: "profile-1", IdempotencyKey: "profile-publish-key", CapsuleRefs: refs,
	})
	if err != nil {
		t.Fatalf("PublishProfileVersion(): %v", err)
	}
	if got, want := version.Snapshot().Digest, domain.ComputeProfileVersionDigest(refs); got != want {
		t.Fatalf("Profile Version digest = %q, want server-computed %q", got, want)
	}
}

type profileRepositoryFake struct {
	createCalls, publishCalls int
	ownerID, profileID        string
	expectedHead              *string
}

func (repository *profileRepositoryFake) CreateProfile(_ context.Context, profile domain.Profile, _ string) (domain.Profile, error) {
	repository.createCalls++
	return profile, nil
}

func (repository *profileRepositoryFake) CheckProfileOwnership(_ context.Context, _, _ string) error {
	return nil
}

func (repository *profileRepositoryFake) PublishProfileVersion(_ context.Context, ownerID, profileID string, expectedHead *string, publication domain.ProfileVersionPublication, _ string) (domain.ProfileVersion, error) {
	repository.publishCalls++
	repository.ownerID, repository.profileID, repository.expectedHead = ownerID, profileID, expectedHead
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: profileID, OwnerUserID: ownerID, Name: "Personal", Slug: "personal", CreatedAt: publication.CreatedAt})
	if err != nil {
		return domain.ProfileVersion{}, err
	}
	return profile.PublishVersion(nil, nil, publication)
}
