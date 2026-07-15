package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestProfileServiceOwnsProfileAndPublicationIdentity(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &profileRepositoryFake{}
	verifier := &recordingUploadVerifier{sizes: map[string]int64{profileDigest('c'): 42}}
	service := application.NewProfileService(repository, verifier, &idsFake{values: []string{"profile-1", "version-1", "artifact-1"}}, func() time.Time { return now })

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
		Digest: profileDigest('a'), Artifacts: []application.ProfileArtifactInput{{
			Kind: domain.ArtifactAgentInstruction, SourceLocator: "AGENTS.md#$",
			SourceDigest: profileDigest('b'), ContentDigest: profileDigest('c'),
			SizeBytes: 42, Mode: 0o640,
			Sensitivity: domain.SensitivityPrivate, Trust: domain.TrustUserAuthored,
		}},
	})
	if err != nil {
		t.Fatalf("PublishProfileVersion(): %v", err)
	}
	if got := version.Snapshot(); got.ID != "version-1" || got.Artifacts[0].ID != "artifact-1" || got.Artifacts[0].ProfileVersionID != "version-1" || got.Artifacts[0].SizeBytes != 42 || got.Artifacts[0].Mode != 0o640 {
		t.Fatalf("published Profile Version = %#v", got)
	}
	if repository.expectedHead != nil || repository.profileID != "profile-1" || repository.ownerID != "user-1" {
		t.Fatalf("publication repository input = owner:%q Profile:%q head:%v", repository.ownerID, repository.profileID, repository.expectedHead)
	}
	if len(verifier.calls) != 1 || verifier.calls[0].OwnerUserID != "user-1" || verifier.calls[0].Kind != domain.UploadProfileArtifact || verifier.calls[0].Digest != profileDigest('c') {
		t.Fatalf("verified uploads = %#v", verifier.calls)
	}
}

func TestProfileServiceRejectsUnsupportedForkAndMissingIdempotency(t *testing.T) {
	repository := &profileRepositoryFake{}
	service := application.NewProfileService(repository, &recordingUploadVerifier{}, &idsFake{values: []string{"unused"}}, time.Now)
	fork := "version-1"
	if _, err := service.CreateProfile(t.Context(), application.CreateProfileInput{
		OwnerUserID: "user-1", Name: "Fork", ForkedFromVersionID: &fork, IdempotencyKey: "profile-create-key",
	}); !errors.Is(err, application.ErrProfileForkUnsupported) {
		t.Fatalf("fork error = %v", err)
	}
	if _, err := service.PublishProfileVersion(t.Context(), application.PublishProfileVersionInput{
		OwnerUserID: "user-1", ProfileID: "profile-1", IdempotencyKey: " ", Digest: profileDigest('a'),
	}); !errors.Is(err, application.ErrInvalidProfileCommand) {
		t.Fatalf("missing idempotency error = %v", err)
	}
	if repository.createCalls != 0 || repository.publishCalls != 0 {
		t.Fatalf("invalid commands reached persistence: create=%d publish=%d", repository.createCalls, repository.publishCalls)
	}
}

func TestProfileServiceRejectsUnverifiedOrWrongSizedArtifactBeforePersistence(t *testing.T) {
	for _, test := range []struct {
		name      string
		size      int64
		verifyErr error
	}{
		{name: "wrong size", size: 41},
		{name: "verification failed", size: 42, verifyErr: errors.New("object store unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &profileRepositoryFake{}
			verifier := &recordingUploadVerifier{sizes: map[string]int64{profileDigest('c'): test.size}, err: test.verifyErr}
			service := application.NewProfileService(repository, verifier, &idsFake{values: []string{"version-1", "artifact-1"}}, time.Now)
			_, err := service.PublishProfileVersion(t.Context(), application.PublishProfileVersionInput{
				OwnerUserID: "user-1", ProfileID: "profile-1", IdempotencyKey: "profile-publish-key", Digest: profileDigest('a'),
				Artifacts: []application.ProfileArtifactInput{{
					Kind: domain.ArtifactAgentInstruction, SourceLocator: "AGENTS.md#$", SourceDigest: profileDigest('b'), ContentDigest: profileDigest('c'),
					SizeBytes: 42, Mode: 0o640, Sensitivity: domain.SensitivityPrivate, Trust: domain.TrustUserAuthored,
				}},
			})
			if err == nil || repository.publishCalls != 0 {
				t.Fatalf("PublishProfileVersion() = calls:%d error:%v", repository.publishCalls, err)
			}
		})
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

func (repository *profileRepositoryFake) PublishProfileVersion(_ context.Context, ownerID, profileID string, expectedHead *string, publication domain.ProfileVersionPublication, _ string) (domain.ProfileVersion, error) {
	repository.publishCalls++
	repository.ownerID, repository.profileID, repository.expectedHead = ownerID, profileID, expectedHead
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{ID: profileID, OwnerUserID: ownerID, Name: "Personal", Slug: "personal", CreatedAt: publication.CreatedAt})
	if err != nil {
		return domain.ProfileVersion{}, err
	}
	return profile.PublishVersion(nil, nil, publication)
}

func profileDigest(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return "sha256:" + string(value)
}
