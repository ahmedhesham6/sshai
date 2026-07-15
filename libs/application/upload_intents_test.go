package application_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestUploadIntentServiceReservesCanonicalIntentBeforeSigning(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	canonical, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-existing", OwnerUserID: "user-1", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('a'), SizeBytes: 12, ObjectKey: testUploadObjectKey("user-1", "upload-existing", domain.UploadProfileArtifact),
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(9 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &uploadRepositoryFake{reserved: canonical}
	signer := &uploadSignerFake{signed: application.SignedUpload{
		URL: "https://objects.example/upload-existing",
		RequiredHeaders: map[string]string{
			"Content-Length":        "12",
			"X-Amz-Checksum-Sha256": "checksum",
		},
	}}
	service := newUploadIntentService(repository, signer, nil, nil, now)

	result, err := service.Create(t.Context(), application.CreateUploadIntentInput{
		OwnerUserID: "user-1", IdempotencyKey: "intent-key-1", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('a'), SizeBytes: 12,
	})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	if result.Intent.Snapshot().ID != "upload-existing" || result.URL != signer.signed.URL || result.RequiredHeaders["Content-Length"] != "12" || result.RequiredHeaders["X-Amz-Checksum-Sha256"] != "checksum" {
		t.Fatalf("result = %#v", result)
	}
	if signer.intent.Snapshot().ID != "upload-existing" || repository.candidate.Snapshot().ID != "upload-new" || repository.key != "intent-key-1" {
		t.Fatalf("repository/signer = candidate:%#v key:%q signed:%#v", repository.candidate.Snapshot(), repository.key, signer.intent.Snapshot())
	}
}

func TestUploadIntentServiceRejectsMismatchedCanonicalReservationBeforeSigning(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	foreign, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-foreign", OwnerUserID: "user-2", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('f'), SizeBytes: 12,
		ObjectKey: testUploadObjectKey("user-2", "upload-foreign", domain.UploadProfileArtifact),
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := &uploadSignerFake{signed: application.SignedUpload{URL: "https://objects.example/foreign"}}
	service := newUploadIntentService(&uploadRepositoryFake{reserved: foreign}, signer, nil, nil, now)
	_, err = service.Create(t.Context(), application.CreateUploadIntentInput{
		OwnerUserID: "user-1", IdempotencyKey: "key", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('a'), SizeBytes: 12,
	})
	if !errors.Is(err, application.ErrUploadReservationMismatch) {
		t.Fatalf("Create() error = %v", err)
	}
	if signer.calls != 0 {
		t.Fatal("mismatched reservation reached signer")
	}
}

func TestUploadIntentServiceRejectsNonCanonicalStagingKeyBeforeSigning(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	invalid, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-existing", OwnerUserID: "user-1", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('a'), SizeBytes: 12, ObjectKey: "uploads/profile_artifact/not-canonical",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := &uploadSignerFake{signed: application.SignedUpload{URL: "https://objects.example/unsafe"}}
	service := newUploadIntentService(&uploadRepositoryFake{reserved: invalid}, signer, nil, nil, now)
	_, err = service.Create(t.Context(), application.CreateUploadIntentInput{
		OwnerUserID: "user-1", IdempotencyKey: "key", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('a'), SizeBytes: 12,
	})
	if !errors.Is(err, application.ErrUploadReservationMismatch) || signer.calls != 0 {
		t.Fatalf("Create() = signer calls:%d error:%v", signer.calls, err)
	}
}

func TestUploadIntentServiceRejectsInvalidOrOversizedInputBeforePersistence(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	for _, input := range []application.CreateUploadIntentInput{
		{OwnerUserID: "user-1", Kind: domain.UploadProfileArtifact, Digest: applicationTestDigest('a'), SizeBytes: 1},
		{OwnerUserID: "user-1", IdempotencyKey: "key", Kind: domain.UploadProfileArtifact, Digest: applicationTestDigest('a'), SizeBytes: 101},
		{OwnerUserID: "user-1", IdempotencyKey: "key", Kind: "unknown", Digest: applicationTestDigest('a'), SizeBytes: 1},
	} {
		repository, signer := &uploadRepositoryFake{}, &uploadSignerFake{}
		service := newUploadIntentService(repository, signer, nil, nil, now)
		if _, err := service.Create(t.Context(), input); !errors.Is(err, application.ErrInvalidUploadIntent) {
			t.Fatalf("Create(%#v) error = %v", input, err)
		}
		if repository.reserveCalls != 0 || signer.calls != 0 {
			t.Fatal("invalid input reached persistence or signer")
		}
	}
}

func TestUploadIntentServiceRejectsUnsafeSignerURL(t *testing.T) {
	repository := &uploadRepositoryFake{}
	signer := &uploadSignerFake{signed: application.SignedUpload{URL: "http://objects.example/upload"}}
	service := newUploadIntentService(repository, signer, nil, nil, time.Now())
	_, err := service.Create(t.Context(), application.CreateUploadIntentInput{
		OwnerUserID: "user-1", IdempotencyKey: "key", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('a'), SizeBytes: 1,
	})
	if !errors.Is(err, application.ErrUnsafeUploadURL) {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestUploadIntentServiceRejectsIncompleteSignedRequest(t *testing.T) {
	repository := &uploadRepositoryFake{}
	signer := &uploadSignerFake{signed: application.SignedUpload{URL: "https://objects.example/upload"}}
	service := newUploadIntentService(repository, signer, nil, nil, time.Now())
	_, err := service.Create(t.Context(), application.CreateUploadIntentInput{
		OwnerUserID: "user-1", IdempotencyKey: "key", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('a'), SizeBytes: 1,
	})
	if !errors.Is(err, application.ErrUnsafeUploadRequest) {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestUploadIntentServiceVerifiesOwnedImmutableObjectMetadata(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-1", OwnerUserID: "user-1", Kind: domain.UploadTrackedPatch,
		Digest: applicationTestDigest('b'), SizeBytes: 37, ObjectKey: "uploads/tracked_patch/object",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &uploadRepositoryFake{loaded: intent}
	inspector := &uploadInspectorFake{object: application.ObservedUpload{
		ObjectKey: "uploads/tracked_patch/object", Kind: domain.UploadTrackedPatch,
		Digest: applicationTestDigest('b'), SizeBytes: 37, VersionID: "object-version-1",
	}}
	finalizer := &uploadFinalizerFake{}
	service := newUploadIntentService(repository, &uploadSignerFake{}, inspector, finalizer, now)
	verified, err := service.Verify(t.Context(), application.VerifyUploadInput{
		OwnerUserID: "user-1", Kind: domain.UploadTrackedPatch, Digest: applicationTestDigest('b'),
	})
	if err != nil {
		t.Fatalf("Verify(): %v", err)
	}
	if verified.Intent.Snapshot().ID != "upload-1" || repository.owner != "user-1" || repository.kind != domain.UploadTrackedPatch || repository.digest != applicationTestDigest('b') || inspector.objectKey != "uploads/tracked_patch/object" {
		t.Fatalf("verification = intent:%#v lookup:%q/%q/%q object:%q", verified.Intent.Snapshot(), repository.owner, repository.kind, repository.digest, inspector.objectKey)
	}
	if finalizer.observed.VersionID != "object-version-1" || finalizer.finalKey == "" || finalizer.finalKey == inspector.objectKey || verified.ObjectKey != finalizer.finalKey {
		t.Fatalf("finalization = observed:%#v key:%q result:%#v", finalizer.observed, finalizer.finalKey, verified)
	}
}

func TestUploadIntentServiceRejectsMetadataMismatchAndLateUpload(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-1", OwnerUserID: "user-1", Kind: domain.UploadSeedManifest,
		Digest: applicationTestDigest('c'), SizeBytes: 10, ObjectKey: "uploads/seed_manifest/object",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		input  application.VerifyUploadInput
		object application.ObservedUpload
	}{
		{name: "intent used for other digest", input: application.VerifyUploadInput{OwnerUserID: "user-1", Kind: domain.UploadSeedManifest, Digest: applicationTestDigest('d')}},
		{name: "object size mismatch", input: application.VerifyUploadInput{OwnerUserID: "user-1", Kind: domain.UploadSeedManifest, Digest: applicationTestDigest('c')}, object: application.ObservedUpload{ObjectKey: "uploads/seed_manifest/object", Kind: domain.UploadSeedManifest, Digest: applicationTestDigest('c'), SizeBytes: 9, VersionID: "v1"}},
		{name: "missing immutable source version", input: application.VerifyUploadInput{OwnerUserID: "user-1", Kind: domain.UploadSeedManifest, Digest: applicationTestDigest('c')}, object: application.ObservedUpload{ObjectKey: "uploads/seed_manifest/object", Kind: domain.UploadSeedManifest, Digest: applicationTestDigest('c'), SizeBytes: 10}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspector := &uploadInspectorFake{object: test.object}
			finalizer := &uploadFinalizerFake{}
			service := newUploadIntentService(&uploadRepositoryFake{loaded: intent}, &uploadSignerFake{}, inspector, finalizer, now)
			if _, err := service.Verify(t.Context(), test.input); !errors.Is(err, application.ErrUploadNotVerified) {
				t.Fatalf("Verify() error = %v", err)
			}
			if finalizer.calls != 0 {
				t.Fatal("invalid upload reached immutable finalization")
			}
		})
	}
}

func TestUploadIntentServicePreservesFinalizationFailure(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-1", OwnerUserID: "user-1", Kind: domain.UploadProfileArtifact,
		Digest: applicationTestDigest('e'), SizeBytes: 4, ObjectKey: "uploads/profile_artifact/staging",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("object store unavailable")
	service := newUploadIntentService(
		&uploadRepositoryFake{loaded: intent}, &uploadSignerFake{},
		&uploadInspectorFake{object: application.ObservedUpload{
			ObjectKey: "uploads/profile_artifact/staging", Kind: domain.UploadProfileArtifact,
			Digest: applicationTestDigest('e'), SizeBytes: 4, VersionID: "v1",
		}},
		&uploadFinalizerFake{err: want}, now,
	)
	if _, err := service.Verify(t.Context(), application.VerifyUploadInput{
		OwnerUserID: "user-1", Kind: domain.UploadProfileArtifact, Digest: applicationTestDigest('e'),
	}); !errors.Is(err, want) {
		t.Fatalf("Verify() error = %v", err)
	}
}

func newUploadIntentService(repository application.UploadIntentRepository, signer application.UploadSigner, inspector application.UploadInspector, finalizer application.UploadFinalizer, now time.Time) *application.UploadIntentService {
	return application.NewUploadIntentService(
		repository, signer, inspector, finalizer, &projectSeedIDs{value: "upload-new"}, func() time.Time { return now },
		10*time.Minute, map[domain.UploadKind]int64{domain.UploadProfileArtifact: 100, domain.UploadTrackedPatch: 100, domain.UploadSeedManifest: 100},
	)
}

type uploadRepositoryFake struct {
	candidate    domain.UploadIntent
	reserved     domain.UploadIntent
	loaded       domain.UploadIntent
	key, owner   string
	kind         domain.UploadKind
	digest       string
	reserveCalls int
}

func (repository *uploadRepositoryFake) ReserveUploadIntent(_ context.Context, candidate domain.UploadIntent, key string) (domain.UploadIntent, error) {
	repository.reserveCalls++
	repository.candidate, repository.key = candidate, key
	if repository.reserved.Snapshot().ID == "" {
		return candidate, nil
	}
	return repository.reserved, nil
}

func (repository *uploadRepositoryFake) GetOwnedUploadIntentByDigest(_ context.Context, owner string, kind domain.UploadKind, digest string) (domain.UploadIntent, error) {
	repository.owner, repository.kind, repository.digest = owner, kind, digest
	return repository.loaded, nil
}

type uploadSignerFake struct {
	intent domain.UploadIntent
	signed application.SignedUpload
	calls  int
}

func (signer *uploadSignerFake) SignUpload(_ context.Context, intent domain.UploadIntent) (application.SignedUpload, error) {
	signer.calls++
	signer.intent = intent
	return signer.signed, nil
}

type uploadInspectorFake struct {
	objectKey string
	object    application.ObservedUpload
}

func (inspector *uploadInspectorFake) InspectUpload(_ context.Context, objectKey string) (application.ObservedUpload, error) {
	inspector.objectKey = objectKey
	return inspector.object, nil
}

type uploadFinalizerFake struct {
	observed application.ObservedUpload
	finalKey string
	calls    int
	err      error
}

func (finalizer *uploadFinalizerFake) FinalizeUpload(_ context.Context, observed application.ObservedUpload, finalKey string) error {
	finalizer.calls++
	finalizer.observed, finalizer.finalKey = observed, finalKey
	return finalizer.err
}

func testUploadObjectKey(ownerID, uploadID string, kind domain.UploadKind) string {
	digest := sha256.Sum256([]byte(ownerID + "\x00" + uploadID))
	return fmt.Sprintf("uploads/%s/%x", kind, digest)
}
