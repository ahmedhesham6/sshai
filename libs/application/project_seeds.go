package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

var ErrInvalidProjectSeed = errors.New("invalid Project Seed command")

type ProjectSeedRepository interface {
	RegisterProjectSeed(context.Context, domain.ProjectSeed, string) (domain.ProjectSeed, error)
}

type RegisterProjectSeedInput struct {
	OwnerUserID           string
	IdempotencyKey        string
	RepositoryURL         string
	BaseRevision          string
	Digest                string
	GitBundleDigest       string
	TrackedPatchDigest    string
	UntrackedBundleDigest string
	ManifestDigest        string
}

type RegisterProjectSeedService struct {
	repository ProjectSeedRepository
	uploads    UploadReferenceVerifier
	ids        IDGenerator
	now        func() time.Time
}

func NewRegisterProjectSeedService(repository ProjectSeedRepository, uploads UploadReferenceVerifier, ids IDGenerator, now func() time.Time) *RegisterProjectSeedService {
	return &RegisterProjectSeedService{repository: repository, uploads: uploads, ids: ids, now: now}
}

func (service *RegisterProjectSeedService) RegisterProjectSeed(ctx context.Context, input RegisterProjectSeedInput) (domain.ProjectSeed, error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return domain.ProjectSeed{}, fmt.Errorf("accept Project Seed command: %w: idempotency key is required", ErrInvalidProjectSeed)
	}
	seed, err := domain.RegisterProjectSeed(domain.ProjectSeedSnapshot{
		ID: service.ids.NewID(), OwnerUserID: input.OwnerUserID, RepositoryURL: input.RepositoryURL,
		BaseRevision: input.BaseRevision, Digest: input.Digest,
		GitBundleDigest: input.GitBundleDigest, TrackedPatchDigest: input.TrackedPatchDigest,
		UntrackedBundleDigest: input.UntrackedBundleDigest, ManifestDigest: input.ManifestDigest,
		CreatedAt: service.now(),
	})
	if err != nil {
		return domain.ProjectSeed{}, fmt.Errorf("accept Project Seed command: %w: %v", ErrInvalidProjectSeed, err)
	}
	if service.uploads == nil {
		return domain.ProjectSeed{}, ErrUploadNotVerified
	}
	parts := []struct {
		kind   domain.UploadKind
		digest string
	}{
		{kind: domain.UploadSeedManifest, digest: input.ManifestDigest},
		{kind: domain.UploadGitBundle, digest: input.GitBundleDigest},
		{kind: domain.UploadTrackedPatch, digest: input.TrackedPatchDigest},
		{kind: domain.UploadUntrackedBundle, digest: input.UntrackedBundleDigest},
	}
	for _, part := range parts {
		if part.digest == "" {
			continue
		}
		if _, err := service.uploads.Verify(ctx, VerifyUploadInput{OwnerUserID: input.OwnerUserID, Kind: part.kind, Digest: part.digest}); err != nil {
			return domain.ProjectSeed{}, fmt.Errorf("verify Project Seed %s: %w", part.kind, err)
		}
	}
	seed, err = service.repository.RegisterProjectSeed(ctx, seed, input.IdempotencyKey)
	if err != nil {
		return domain.ProjectSeed{}, fmt.Errorf("persist Project Seed: %w", err)
	}
	return seed, nil
}
