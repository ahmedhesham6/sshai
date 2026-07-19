package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

var (
	ErrInvalidUploadIntent       = errors.New("invalid Upload Intent")
	ErrUnsafeUploadURL           = errors.New("unsafe upload URL")
	ErrUnsafeUploadRequest       = errors.New("unsafe signed upload request")
	ErrUploadObjectNotFound      = errors.New("upload object not found")
	ErrUploadNotVerified         = errors.New("upload not verified")
	ErrUploadReservationMismatch = errors.New("Upload Intent reservation mismatch")
)

type UploadIntentRepository interface {
	ReserveUploadIntent(context.Context, domain.UploadIntent, string) (domain.UploadIntent, error)
	GetOwnedUploadIntentByDigest(context.Context, string, domain.UploadKind, string) (domain.UploadIntent, error)
}

type UploadSigner interface {
	SignUpload(context.Context, domain.UploadIntent) (SignedUpload, error)
}

type SignedUpload struct {
	URL             string
	RequiredHeaders map[string]string
}

type UploadInspector interface {
	InspectUpload(context.Context, string) (ObservedUpload, error)
}

type UploadFinalizer interface {
	// FinalizeUpload must promote exactly observed.VersionID to finalObjectKey,
	// never grant clients write access to that key, and make retries converge
	// without replacing different content.
	FinalizeUpload(context.Context, ObservedUpload, string) error
}

type UploadReferenceVerifier interface {
	Verify(context.Context, VerifyUploadInput) (VerifiedUpload, error)
}

type ObservedUpload struct {
	ObjectKey string
	Kind      domain.UploadKind
	Digest    string
	SizeBytes int64
	VersionID string
}

type CreateUploadIntentInput struct {
	OwnerUserID    string
	IdempotencyKey string
	Kind           domain.UploadKind
	Digest         string
	SizeBytes      int64
}

type UploadIntentResult struct {
	Intent          domain.UploadIntent
	URL             string
	RequiredHeaders map[string]string
}

type VerifiedUpload struct {
	Intent    domain.UploadIntent
	ObjectKey string
}

type VerifyUploadInput struct {
	OwnerUserID string
	Kind        domain.UploadKind
	Digest      string
}

type UploadIntentService struct {
	repository UploadIntentRepository
	signer     UploadSigner
	inspector  UploadInspector
	finalizer  UploadFinalizer
	ids        IDGenerator
	now        func() time.Time
	ttl        time.Duration
	limits     map[domain.UploadKind]int64
}

func NewUploadIntentService(repository UploadIntentRepository, signer UploadSigner, inspector UploadInspector, finalizer UploadFinalizer, ids IDGenerator, now func() time.Time, ttl time.Duration, limits map[domain.UploadKind]int64) *UploadIntentService {
	configuredLimits := make(map[domain.UploadKind]int64, len(limits))
	for kind, limit := range limits {
		configuredLimits[kind] = limit
	}
	return &UploadIntentService{repository: repository, signer: signer, inspector: inspector, finalizer: finalizer, ids: ids, now: now, ttl: ttl, limits: configuredLimits}
}

func (service *UploadIntentService) Create(ctx context.Context, input CreateUploadIntentInput) (UploadIntentResult, error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" || service.repository == nil || service.signer == nil || service.ids == nil || service.now == nil || service.ttl <= 0 {
		return UploadIntentResult{}, ErrInvalidUploadIntent
	}
	limit, configured := service.limits[input.Kind]
	if !configured || limit < 0 || input.SizeBytes > limit {
		return UploadIntentResult{}, ErrInvalidUploadIntent
	}
	createdAt := service.now().UTC().Truncate(time.Second)
	id := service.ids.NewID()
	candidate, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: id, OwnerUserID: input.OwnerUserID, Kind: input.Kind, Digest: input.Digest,
		SizeBytes: input.SizeBytes, ObjectKey: uploadObjectKey(input.OwnerUserID, id, input.Kind),
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(service.ttl),
	})
	if err != nil {
		return UploadIntentResult{}, fmt.Errorf("create Upload Intent: %w: %v", ErrInvalidUploadIntent, err)
	}
	intent, err := service.repository.ReserveUploadIntent(ctx, candidate, input.IdempotencyKey)
	if err != nil {
		return UploadIntentResult{}, fmt.Errorf("create Upload Intent: reserve: %w", err)
	}
	reserved := intent.Snapshot()
	if reserved.OwnerUserID != input.OwnerUserID || reserved.Kind != input.Kind || reserved.Digest != input.Digest ||
		reserved.SizeBytes != input.SizeBytes || reserved.ObjectKey != uploadObjectKey(reserved.OwnerUserID, reserved.ID, reserved.Kind) {
		return UploadIntentResult{}, ErrUploadReservationMismatch
	}
	signed, err := service.signer.SignUpload(ctx, intent)
	if err != nil {
		return UploadIntentResult{}, fmt.Errorf("create Upload Intent: sign: %w", err)
	}
	parsed, err := url.Parse(signed.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return UploadIntentResult{}, ErrUnsafeUploadURL
	}
	if !safeSignedHeaders(signed.RequiredHeaders) {
		return UploadIntentResult{}, ErrUnsafeUploadRequest
	}
	return UploadIntentResult{Intent: intent, URL: parsed.String(), RequiredHeaders: maps.Clone(signed.RequiredHeaders)}, nil
}

func safeSignedHeaders(headers map[string]string) bool {
	if len(headers) == 0 || len(headers) > 16 {
		return false
	}
	total := 0
	for name, value := range headers {
		total += len(name) + len(value)
		if !validSignedHeaderName(name) || strings.ContainsAny(value, "\r\n") || total > 8<<10 {
			return false
		}
	}
	return true
}

func validSignedHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if character <= 32 || character >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={} ", character) {
			return false
		}
	}
	return true
}

func (service *UploadIntentService) Verify(ctx context.Context, input VerifyUploadInput) (VerifiedUpload, error) {
	if service.repository == nil || service.inspector == nil || service.finalizer == nil || input.OwnerUserID == "" {
		return VerifiedUpload{}, ErrUploadNotVerified
	}
	intent, err := service.repository.GetOwnedUploadIntentByDigest(ctx, input.OwnerUserID, input.Kind, input.Digest)
	if err != nil {
		return VerifiedUpload{}, fmt.Errorf("verify upload: load intent: %w", err)
	}
	snapshot := intent.Snapshot()
	if snapshot.OwnerUserID != input.OwnerUserID || snapshot.Kind != input.Kind || snapshot.Digest != input.Digest {
		return VerifiedUpload{}, ErrUploadNotVerified
	}
	observed, err := service.inspector.InspectUpload(ctx, snapshot.ObjectKey)
	if err != nil {
		return VerifiedUpload{}, fmt.Errorf("verify upload: inspect object: %w", err)
	}
	if observed.ObjectKey != snapshot.ObjectKey || observed.Kind != snapshot.Kind || observed.Digest != snapshot.Digest || observed.SizeBytes != snapshot.SizeBytes ||
		observed.VersionID == "" {
		return VerifiedUpload{}, ErrUploadNotVerified
	}
	finalKey := FinalizedUploadObjectKey(snapshot.OwnerUserID, snapshot.Kind, snapshot.Digest)
	if err := service.finalizer.FinalizeUpload(ctx, observed, finalKey); err != nil {
		return VerifiedUpload{}, fmt.Errorf("verify upload: finalize immutable object: %w", err)
	}
	return VerifiedUpload{Intent: intent, ObjectKey: finalKey}, nil
}

// FinalizedUploadObjectKey is the immutable object location shared by upload
// finalization and owner-scoped Project Seed delivery.
func FinalizedUploadObjectKey(ownerID string, kind domain.UploadKind, digest string) string {
	ownerDigest := sha256.Sum256([]byte(ownerID))
	return fmt.Sprintf("objects/%x/%s/%s", ownerDigest, kind, strings.TrimPrefix(digest, "sha256:"))
}

func uploadObjectKey(ownerID, uploadID string, kind domain.UploadKind) string {
	digest := sha256.Sum256([]byte(ownerID + "\x00" + uploadID))
	return fmt.Sprintf("uploads/%s/%x", kind, digest)
}
