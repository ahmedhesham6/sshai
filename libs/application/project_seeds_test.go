package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestRegisterProjectSeedServiceOwnsIdentityAndIdempotentPersistence(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	canonical, err := domain.RegisterProjectSeed(domain.ProjectSeedSnapshot{
		ID: "seed-existing", OwnerUserID: "user-1", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: applicationTestDigest('a'),
		GitBundleDigest: applicationTestDigest('c'), TrackedPatchDigest: applicationTestDigest('d'),
		UntrackedBundleDigest: applicationTestDigest('e'), ManifestDigest: applicationTestDigest('b'),
		CreatedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("create canonical Project Seed: %v", err)
	}
	repository := &projectSeedRepositoryFake{result: canonical}
	verifier := &recordingUploadVerifier{}
	service := application.NewRegisterProjectSeedService(repository, verifier, &projectSeedIDs{value: "seed-1"}, func() time.Time { return now })

	seed, err := service.RegisterProjectSeed(t.Context(), application.RegisterProjectSeedInput{
		OwnerUserID: "user-1", IdempotencyKey: "registration-key-1",
		RepositoryURL: "https://github.com/example/project.git", BaseRevision: "abc123",
		Digest: applicationTestDigest('a'), GitBundleDigest: applicationTestDigest('c'),
		TrackedPatchDigest: applicationTestDigest('d'), UntrackedBundleDigest: applicationTestDigest('e'),
		ManifestDigest: applicationTestDigest('b'),
	})
	if err != nil {
		t.Fatalf("RegisterProjectSeed(): %v", err)
	}
	snapshot := seed.Snapshot()
	if snapshot.ID != "seed-existing" || !snapshot.CreatedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("registered Project Seed = %#v", snapshot)
	}
	candidate := repository.seed.Snapshot()
	if repository.idempotencyKey != "registration-key-1" || candidate.ID != "seed-1" ||
		candidate.GitBundleDigest != applicationTestDigest('c') || candidate.TrackedPatchDigest != applicationTestDigest('d') ||
		candidate.UntrackedBundleDigest != applicationTestDigest('e') {
		t.Fatalf("repository call = key:%q seed:%#v", repository.idempotencyKey, repository.seed.Snapshot())
	}
	wantKinds := []domain.UploadKind{domain.UploadSeedManifest, domain.UploadGitBundle, domain.UploadTrackedPatch, domain.UploadUntrackedBundle}
	if len(verifier.calls) != len(wantKinds) {
		t.Fatalf("verified uploads = %#v", verifier.calls)
	}
	for index, kind := range wantKinds {
		if verifier.calls[index].OwnerUserID != "user-1" || verifier.calls[index].Kind != kind {
			t.Fatalf("verified upload %d = %#v", index, verifier.calls[index])
		}
	}
}

func TestRegisterProjectSeedServiceRejectsMissingIdempotencyBeforePersistence(t *testing.T) {
	repository := &projectSeedRepositoryFake{}
	service := application.NewRegisterProjectSeedService(repository, &recordingUploadVerifier{}, &projectSeedIDs{value: "seed-1"}, time.Now)
	_, err := service.RegisterProjectSeed(t.Context(), application.RegisterProjectSeedInput{
		OwnerUserID: "user-1", IdempotencyKey: "  ", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: applicationTestDigest('a'), ManifestDigest: applicationTestDigest('b'),
	})
	if err == nil || repository.calls != 0 {
		t.Fatalf("missing idempotency result = calls:%d err:%v", repository.calls, err)
	}
}

func TestRegisterProjectSeedServicePreservesRepositoryError(t *testing.T) {
	want := errors.New("database unavailable")
	repository := &projectSeedRepositoryFake{err: want}
	service := application.NewRegisterProjectSeedService(repository, &recordingUploadVerifier{}, &projectSeedIDs{value: "seed-1"}, time.Now)
	_, err := service.RegisterProjectSeed(t.Context(), application.RegisterProjectSeedInput{
		OwnerUserID: "user-1", IdempotencyKey: "registration-key-1", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: applicationTestDigest('a'), ManifestDigest: applicationTestDigest('b'),
	})
	if !errors.Is(err, want) {
		t.Fatalf("RegisterProjectSeed() error = %v", err)
	}
}

func TestRegisterProjectSeedServiceRejectsInvalidInputBeforePersistence(t *testing.T) {
	repository := &projectSeedRepositoryFake{}
	service := application.NewRegisterProjectSeedService(repository, &recordingUploadVerifier{}, &projectSeedIDs{value: "seed-1"}, time.Now)
	_, err := service.RegisterProjectSeed(t.Context(), application.RegisterProjectSeedInput{
		OwnerUserID: "user-1", IdempotencyKey: "registration-key-1", RepositoryURL: "file:///tmp/project",
		BaseRevision: "abc123", Digest: applicationTestDigest('a'), ManifestDigest: applicationTestDigest('b'),
	})
	if !errors.Is(err, application.ErrInvalidProjectSeed) {
		t.Fatalf("invalid Project Seed error = %v", err)
	}
	if repository.calls != 0 {
		t.Fatalf("invalid input reached persistence %d times", repository.calls)
	}
}

func TestRegisterProjectSeedServiceVerifiesOnlyPresentPartsBeforePersistence(t *testing.T) {
	repository := &projectSeedRepositoryFake{}
	verifier := &recordingUploadVerifier{}
	service := application.NewRegisterProjectSeedService(repository, verifier, &projectSeedIDs{value: "seed-1"}, time.Now)
	_, err := service.RegisterProjectSeed(t.Context(), application.RegisterProjectSeedInput{
		OwnerUserID: "user-1", IdempotencyKey: "registration-key-1", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: applicationTestDigest('a'), TrackedPatchDigest: applicationTestDigest('c'), ManifestDigest: applicationTestDigest('b'),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier.calls) != 2 || verifier.calls[0].Kind != domain.UploadSeedManifest || verifier.calls[1].Kind != domain.UploadTrackedPatch {
		t.Fatalf("verified uploads = %#v", verifier.calls)
	}
}

func TestRegisterProjectSeedServiceRejectsVerifierFailureBeforePersistence(t *testing.T) {
	want := errors.New("object store unavailable")
	repository := &projectSeedRepositoryFake{}
	service := application.NewRegisterProjectSeedService(repository, &recordingUploadVerifier{err: want}, &projectSeedIDs{value: "seed-1"}, time.Now)
	_, err := service.RegisterProjectSeed(t.Context(), application.RegisterProjectSeedInput{
		OwnerUserID: "user-1", IdempotencyKey: "registration-key-1", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: applicationTestDigest('a'), ManifestDigest: applicationTestDigest('b'),
	})
	if !errors.Is(err, want) || repository.calls != 0 {
		t.Fatalf("RegisterProjectSeed() = calls:%d error:%v", repository.calls, err)
	}
}

func TestRegisterProjectSeedServiceRejectsAggregateContentAboveTransportLimit(t *testing.T) {
	repository := &projectSeedRepositoryFake{}
	manifestDigest := applicationTestDigest('b')
	trackedDigest := applicationTestDigest('c')
	verifier := &recordingUploadVerifier{sizes: map[string]int64{
		manifestDigest: application.ProjectSeedTransportMaximumRawBytes/2 + 1,
		trackedDigest:  application.ProjectSeedTransportMaximumRawBytes/2 + 1,
	}}
	service := application.NewRegisterProjectSeedService(repository, verifier, &projectSeedIDs{value: "seed-1"}, time.Now)
	_, err := service.RegisterProjectSeed(t.Context(), application.RegisterProjectSeedInput{
		OwnerUserID: "user-1", IdempotencyKey: "registration-key-1", RepositoryURL: "https://github.com/example/project.git",
		BaseRevision: "abc123", Digest: applicationTestDigest('a'), ManifestDigest: manifestDigest, TrackedPatchDigest: trackedDigest,
	})
	if !errors.Is(err, application.ErrProjectSeedTransportLimit) || repository.calls != 0 {
		t.Fatalf("oversize registration = calls:%d error:%v", repository.calls, err)
	}
}

type projectSeedRepositoryFake struct {
	seed           domain.ProjectSeed
	idempotencyKey string
	calls          int
	result         domain.ProjectSeed
	err            error
}

func (repository *projectSeedRepositoryFake) RegisterProjectSeed(_ context.Context, seed domain.ProjectSeed, idempotencyKey string) (domain.ProjectSeed, error) {
	repository.calls++
	repository.seed, repository.idempotencyKey = seed, idempotencyKey
	if repository.result.Snapshot().ID == "" {
		repository.result = seed
	}
	return repository.result, repository.err
}

type projectSeedIDs struct{ value string }

func (ids *projectSeedIDs) NewID() string { return ids.value }

func applicationTestDigest(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return "sha256:" + string(value)
}
