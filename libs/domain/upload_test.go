package domain_test

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestReserveUploadIntentAcceptsClosedKindsAndCopiesSnapshot(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	for _, kind := range []domain.UploadKind{
		domain.UploadProfileArtifact, domain.UploadGitBundle, domain.UploadTrackedPatch,
		domain.UploadUntrackedBundle, domain.UploadSeedManifest,
	} {
		t.Run(string(kind), func(t *testing.T) {
			intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
				ID: "upload-1", OwnerUserID: "user-1", Kind: kind,
				Digest: uploadTestDigest('a'), SizeBytes: 42, ObjectKey: "uploads/object-1",
				CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
			})
			if err != nil {
				t.Fatalf("ReserveUploadIntent(): %v", err)
			}
			got := intent.Snapshot()
			if got.Kind != kind || got.Digest != uploadTestDigest('a') || got.SizeBytes != 42 || got.ObjectKey != "uploads/object-1" {
				t.Fatalf("snapshot = %#v", got)
			}
		})
	}
}

func TestReserveUploadIntentRejectsInvalidIdentityContentAddressAndLifetime(t *testing.T) {
	now := time.Date(2026, time.July, 13, 6, 0, 0, 0, time.UTC)
	valid := domain.UploadIntentSnapshot{
		ID: "upload-1", OwnerUserID: "user-1", Kind: domain.UploadProfileArtifact,
		Digest: uploadTestDigest('a'), SizeBytes: 1, ObjectKey: "uploads/object-1",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	tests := []struct {
		name   string
		mutate func(*domain.UploadIntentSnapshot)
	}{
		{name: "missing ID", mutate: func(value *domain.UploadIntentSnapshot) { value.ID = "" }},
		{name: "blank ID", mutate: func(value *domain.UploadIntentSnapshot) { value.ID = "  " }},
		{name: "missing owner", mutate: func(value *domain.UploadIntentSnapshot) { value.OwnerUserID = "" }},
		{name: "blank owner", mutate: func(value *domain.UploadIntentSnapshot) { value.OwnerUserID = "  " }},
		{name: "unknown kind", mutate: func(value *domain.UploadIntentSnapshot) { value.Kind = "archive" }},
		{name: "malformed digest", mutate: func(value *domain.UploadIntentSnapshot) { value.Digest = "sha256:nope" }},
		{name: "negative size", mutate: func(value *domain.UploadIntentSnapshot) { value.SizeBytes = -1 }},
		{name: "missing object key", mutate: func(value *domain.UploadIntentSnapshot) { value.ObjectKey = "" }},
		{name: "blank object key", mutate: func(value *domain.UploadIntentSnapshot) { value.ObjectKey = "  " }},
		{name: "missing creation", mutate: func(value *domain.UploadIntentSnapshot) { value.CreatedAt = time.Time{} }},
		{name: "missing expiry", mutate: func(value *domain.UploadIntentSnapshot) { value.ExpiresAt = time.Time{} }},
		{name: "expiry not after creation", mutate: func(value *domain.UploadIntentSnapshot) { value.ExpiresAt = now }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if _, err := domain.ReserveUploadIntent(candidate); err == nil {
				t.Fatal("ReserveUploadIntent() error = nil")
			}
		})
	}
}

func uploadTestDigest(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return "sha256:" + string(value)
}
