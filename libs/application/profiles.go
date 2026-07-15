package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

var (
	ErrInvalidProfileCommand  = errors.New("invalid Profile command")
	ErrProfileForkUnsupported = errors.New("Profile fork is unsupported")
)

type ProfileRepository interface {
	CreateProfile(context.Context, domain.Profile, string) (domain.Profile, error)
	PublishProfileVersion(context.Context, string, string, *string, domain.ProfileVersionPublication, string) (domain.ProfileVersion, error)
}

type CreateProfileInput struct {
	OwnerUserID         string
	Name                string
	ForkedFromVersionID *string
	IdempotencyKey      string
}

type ProfileArtifactInput struct {
	Kind               domain.ArtifactKind
	SourceLocator      string
	SourceDigest       string
	ContentDigest      string
	SizeBytes          int64
	Mode               uint32
	Sensitivity        domain.Sensitivity
	Trust              domain.TrustClass
	ContainsExecutable bool
}

type PublishProfileVersionInput struct {
	OwnerUserID           string
	ProfileID             string
	ExpectedHeadVersionID *string
	Digest                string
	Artifacts             []ProfileArtifactInput
	IdempotencyKey        string
}

type ProfileService struct {
	repository ProfileRepository
	uploads    UploadReferenceVerifier
	ids        IDGenerator
	now        func() time.Time
}

func NewProfileService(repository ProfileRepository, uploads UploadReferenceVerifier, ids IDGenerator, now func() time.Time) *ProfileService {
	return &ProfileService{repository: repository, uploads: uploads, ids: ids, now: now}
}

func (service *ProfileService) CreateProfile(ctx context.Context, input CreateProfileInput) (domain.Profile, error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return domain.Profile{}, fmt.Errorf("create Profile: %w: idempotency key is required", ErrInvalidProfileCommand)
	}
	if input.ForkedFromVersionID != nil {
		return domain.Profile{}, ErrProfileForkUnsupported
	}
	profile, err := domain.CreateProfile(domain.ProfileSnapshot{
		ID: service.ids.NewID(), OwnerUserID: input.OwnerUserID, Name: input.Name,
		Slug: slugify(input.Name), CreatedAt: service.now(),
	})
	if err != nil {
		return domain.Profile{}, fmt.Errorf("create Profile: %w: %v", ErrInvalidProfileCommand, err)
	}
	profile, err = service.repository.CreateProfile(ctx, profile, input.IdempotencyKey)
	if err != nil {
		return domain.Profile{}, fmt.Errorf("create Profile: persist: %w", err)
	}
	return profile, nil
}

func (service *ProfileService) PublishProfileVersion(ctx context.Context, input PublishProfileVersionInput) (domain.ProfileVersion, error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: %w: idempotency key is required", ErrInvalidProfileCommand)
	}
	versionID := service.ids.NewID()
	artifacts := make([]domain.ProfileArtifact, len(input.Artifacts))
	for index, artifact := range input.Artifacts {
		artifacts[index] = domain.ProfileArtifact{
			ID: service.ids.NewID(), ProfileVersionID: versionID, Kind: artifact.Kind,
			SourceLocator: artifact.SourceLocator, SourceDigest: artifact.SourceDigest,
			ContentDigest: artifact.ContentDigest, SizeBytes: artifact.SizeBytes, Mode: artifact.Mode, Sensitivity: artifact.Sensitivity,
			Trust: artifact.Trust, ContainsExecutable: artifact.ContainsExecutable,
		}
	}
	publication := domain.ProfileVersionPublication{
		ID: versionID, Digest: input.Digest, Artifacts: artifacts, CreatedAt: service.now(),
	}
	if service.uploads == nil {
		return domain.ProfileVersion{}, ErrUploadNotVerified
	}
	for index, artifact := range artifacts {
		verified, err := service.uploads.Verify(ctx, VerifyUploadInput{
			OwnerUserID: input.OwnerUserID, Kind: domain.UploadProfileArtifact, Digest: artifact.ContentDigest,
		})
		if err != nil {
			return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: verify artifact %d: %w", index, err)
		}
		if verified.Intent.Snapshot().SizeBytes != artifact.SizeBytes {
			return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: artifact %d size: %w", index, ErrUploadNotVerified)
		}
	}
	expectedHead := input.ExpectedHeadVersionID
	if expectedHead != nil {
		copy := *expectedHead
		expectedHead = &copy
	}
	version, err := service.repository.PublishProfileVersion(
		ctx, input.OwnerUserID, input.ProfileID, expectedHead, publication, input.IdempotencyKey,
	)
	if err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("publish Profile Version: persist: %w", err)
	}
	return version, nil
}
