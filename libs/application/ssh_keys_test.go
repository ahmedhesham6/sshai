package application_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"golang.org/x/crypto/ssh"
)

func TestSSHKeyServiceCanonicalizesEd25519AndOwnsFingerprint(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	public, fingerprint := applicationSSHKey(t)
	canonical, err := domain.RegisterSSHKey(domain.SSHKeyRegistration{
		ID: "ssh-key-existing", OwnerUserID: "user-1", Label: "Laptop",
		PublicKey: public, CreatedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &sshKeyRepositoryFake{registered: canonical}
	service := application.NewSSHKeyService(repository, &projectSeedIDs{value: "ssh-key-new"}, func() time.Time { return now })
	result, err := service.Register(t.Context(), application.RegisterSSHKeyInput{
		OwnerUserID: "user-1", IdempotencyKey: "register-key", Label: "Laptop", PublicKey: public + " local-comment",
	})
	if err != nil {
		t.Fatalf("Register(): %v", err)
	}
	if result.Snapshot().ID != "ssh-key-existing" || repository.key != "register-key" {
		t.Fatalf("result = %#v key=%q", result.Snapshot(), repository.key)
	}
	candidate := repository.candidate.Snapshot()
	if candidate.ID != "ssh-key-new" || candidate.PublicKey != public || candidate.Fingerprint != fingerprint || strings.Contains(candidate.PublicKey, "local-comment") {
		t.Fatalf("candidate = %#v", candidate)
	}
}

func TestSSHKeyServiceRejectsNonEd25519OptionsMultipleAndMalformedBeforePersistence(t *testing.T) {
	public, _ := applicationSSHKey(t)
	tests := []string{
		"ssh-rsa AAAA",
		`command="bad" ` + public,
		public + "\n" + public,
		"not-a-key",
	}
	for _, value := range tests {
		repository := &sshKeyRepositoryFake{}
		service := application.NewSSHKeyService(repository, &projectSeedIDs{value: "ssh-key"}, time.Now)
		_, err := service.Register(t.Context(), application.RegisterSSHKeyInput{
			OwnerUserID: "user-1", IdempotencyKey: "key", Label: "Laptop", PublicKey: value,
		})
		if !errors.Is(err, application.ErrInvalidSSHKey) || repository.registerCalls != 0 {
			t.Fatalf("Register(%q) = calls:%d error:%v", value, repository.registerCalls, err)
		}
	}
}

func TestSSHKeyServiceRejectsWhitespaceBearingIdentityBeforePersistence(t *testing.T) {
	public, _ := applicationSSHKey(t)
	registerTests := []application.RegisterSSHKeyInput{
		{OwnerUserID: " user-1", IdempotencyKey: "key", Label: "Laptop", PublicKey: public},
		{OwnerUserID: "user-1", IdempotencyKey: "key ", Label: "Laptop", PublicKey: public},
	}
	for _, input := range registerTests {
		repository := &sshKeyRepositoryFake{}
		service := application.NewSSHKeyService(repository, &projectSeedIDs{value: "ssh-key"}, time.Now)
		if _, err := service.Register(t.Context(), input); !errors.Is(err, application.ErrInvalidSSHKey) || repository.registerCalls != 0 {
			t.Fatalf("Register(%#v) = calls:%d error:%v", input, repository.registerCalls, err)
		}
	}

	revokeTests := []application.RevokeSSHKeyInput{
		{OwnerUserID: " user-1", SSHKeyID: "ssh-key-1", IdempotencyKey: "key"},
		{OwnerUserID: "user-1", SSHKeyID: "ssh-key-1 ", IdempotencyKey: "key"},
		{OwnerUserID: "user-1", SSHKeyID: "ssh-key-1", IdempotencyKey: "key "},
	}
	for _, input := range revokeTests {
		repository := &sshKeyRepositoryFake{}
		service := application.NewSSHKeyService(repository, &projectSeedIDs{value: "unused"}, time.Now)
		if err := service.Revoke(t.Context(), input); !errors.Is(err, application.ErrInvalidSSHKey) || repository.revokeCalls != 0 {
			t.Fatalf("Revoke(%#v) = calls:%d error:%v", input, repository.revokeCalls, err)
		}
	}
}

func TestSSHKeyServiceRejectsMismatchedCanonicalRegistration(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	public, _ := applicationSSHKey(t)
	foreign, err := domain.RegisterSSHKey(domain.SSHKeyRegistration{
		ID: "ssh-key-foreign", OwnerUserID: "user-2", Label: "Laptop",
		PublicKey: public, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewSSHKeyService(&sshKeyRepositoryFake{registered: foreign}, &projectSeedIDs{value: "ssh-key-new"}, func() time.Time { return now })
	if _, err := service.Register(t.Context(), application.RegisterSSHKeyInput{
		OwnerUserID: "user-1", IdempotencyKey: "key", Label: "Laptop", PublicKey: public,
	}); !errors.Is(err, application.ErrSSHKeyReservationMismatch) {
		t.Fatalf("Register() error = %v", err)
	}
}

func TestSSHKeyServiceRevokesOwnedKeyIdempotently(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	repository := &sshKeyRepositoryFake{}
	service := application.NewSSHKeyService(repository, &projectSeedIDs{value: "unused"}, func() time.Time { return now })
	if err := service.Revoke(t.Context(), application.RevokeSSHKeyInput{OwnerUserID: "user-1", SSHKeyID: "ssh-key-1", IdempotencyKey: "revoke-key"}); err != nil {
		t.Fatalf("Revoke(): %v", err)
	}
	if repository.owner != "user-1" || repository.id != "ssh-key-1" || repository.key != "revoke-key" || !repository.revokedAt.Equal(now) {
		t.Fatalf("revoke call = %#v", repository)
	}
}

func TestSSHKeyServiceListsOnlyActiveOwnedPublicKeys(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	public, _ := applicationSSHKey(t)
	key, err := domain.RegisterSSHKey(domain.SSHKeyRegistration{
		ID: "ssh-key-1", OwnerUserID: "user-1", Label: "Laptop", PublicKey: public, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &sshKeyRepositoryFake{listed: []domain.SSHKey{key}}
	service := application.NewSSHKeyService(repository, &projectSeedIDs{value: "unused"}, time.Now)
	keys, err := service.List(t.Context(), "user-1")
	if err != nil || len(keys) != 1 || keys[0].Snapshot().ID != "ssh-key-1" || repository.owner != "user-1" {
		t.Fatalf("List() = keys:%#v owner:%q error:%v", keys, repository.owner, err)
	}
}

func TestSSHKeyServiceRejectsRepositoryOwnershipOrRevocationLeak(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	public, _ := applicationSSHKey(t)
	foreign, err := domain.RegisterSSHKey(domain.SSHKeyRegistration{
		ID: "ssh-key-foreign", OwnerUserID: "user-2", Label: "Laptop", PublicKey: public, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := foreign.Revoke(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []domain.SSHKey{foreign, revoked} {
		service := application.NewSSHKeyService(&sshKeyRepositoryFake{listed: []domain.SSHKey{leaked}}, &projectSeedIDs{value: "unused"}, time.Now)
		if _, err := service.List(t.Context(), "user-1"); !errors.Is(err, application.ErrSSHKeyReservationMismatch) {
			t.Fatalf("List() leak error = %v", err)
		}
	}
}

type sshKeyRepositoryFake struct {
	candidate     domain.SSHKey
	registered    domain.SSHKey
	key           string
	owner, id     string
	revokedAt     time.Time
	registerCalls int
	revokeCalls   int
	listed        []domain.SSHKey
}

func (repository *sshKeyRepositoryFake) RegisterSSHKey(_ context.Context, candidate domain.SSHKey, key string) (domain.SSHKey, error) {
	repository.registerCalls++
	repository.candidate, repository.key = candidate, key
	if repository.registered.Snapshot().ID == "" {
		return candidate, nil
	}
	return repository.registered, nil
}

func (repository *sshKeyRepositoryFake) RevokeOwnedSSHKey(_ context.Context, owner, id, key string, revokedAt time.Time) error {
	repository.revokeCalls++
	repository.owner, repository.id, repository.key, repository.revokedAt = owner, id, key, revokedAt
	return nil
}

func (repository *sshKeyRepositoryFake) ListActiveOwnedSSHKeys(_ context.Context, owner string) ([]domain.SSHKey, error) {
	repository.owner = owner
	return repository.listed, nil
}

func applicationSSHKey(t *testing.T) (string, string) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), ssh.FingerprintSHA256(key)
}
