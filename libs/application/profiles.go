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
	CheckProfileOwnership(context.Context, string, string) error
}

type CreateProfileInput struct {
	OwnerUserID         string
	Name                string
	ForkedFromVersionID *string
	IdempotencyKey      string
}

type PublishProfileVersionInput struct {
	OwnerUserID           string
	ProfileID             string
	ExpectedHeadVersionID *string
	CapsuleRefs           []domain.CapsuleRef
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
	publication := domain.ProfileVersionPublication{
		ID: versionID, Digest: domain.ComputeProfileVersionDigest(input.CapsuleRefs),
		CapsuleRefs: cloneCapsuleRefs(input.CapsuleRefs), CreatedAt: service.now(),
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

// ValidateProfileVersionPublication runs the authenticated owner's validation
// chain for the publication stub without persisting a Profile Version.
func (service *ProfileService) ValidateProfileVersionPublication(ctx context.Context, input PublishProfileVersionInput) error {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return fmt.Errorf("publish Profile Version: %w: idempotency key is required", ErrInvalidProfileCommand)
	}
	if err := domain.ValidateCapsuleRefs(input.CapsuleRefs); err != nil {
		return fmt.Errorf("publish Profile Version: %w: %v", ErrInvalidProfileCommand, err)
	}
	if err := service.repository.CheckProfileOwnership(ctx, input.OwnerUserID, input.ProfileID); err != nil {
		return fmt.Errorf("publish Profile Version: check ownership: %w", err)
	}
	return nil
}

func cloneCapsuleRefs(refs []domain.CapsuleRef) []domain.CapsuleRef {
	if refs == nil {
		return nil
	}
	clone := append([]domain.CapsuleRef(nil), refs...)
	for index := range clone {
		clone[index].Exclusions = append([]string(nil), clone[index].Exclusions...)
	}
	return clone
}
