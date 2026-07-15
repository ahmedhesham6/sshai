package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestRegisterAndRevokeSSHKeyPreservesImmutablePublicIdentity(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	key, err := domain.RegisterSSHKey(domain.SSHKeyRegistration{
		ID: "ssh-key-1", OwnerUserID: "user-1", Label: "Work laptop",
		PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMzdhbPIA9osmLQz0iTvx/VNJP8fjiD3wfl9LSn2d92", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("RegisterSSHKey(): %v", err)
	}
	revoked, err := key.Revoke(now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Revoke(): %v", err)
	}
	got := revoked.Snapshot()
	if got.ID != "ssh-key-1" || got.OwnerUserID != "user-1" || got.Algorithm != domain.SSHKeyEd25519 || got.Fingerprint != "SHA256:mhqmgVD6eE8cj8LlLtVABSgyzWTHFDkHMX7Irr3oI0w" || got.PublicKey != key.Snapshot().PublicKey || got.RevokedAt == nil || !got.RevokedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("revoked SSH Key = %#v", got)
	}
	replayed, err := revoked.Revoke(now.Add(2 * time.Minute))
	if err != nil || !replayed.Snapshot().RevokedAt.Equal(*got.RevokedAt) {
		t.Fatalf("replayed revocation = %#v error:%v", replayed.Snapshot(), err)
	}
}

func TestRegisterSSHKeyRejectsInvalidOrSecretBearingState(t *testing.T) {
	now := time.Now()
	valid := domain.SSHKeyRegistration{
		ID: "ssh-key-1", OwnerUserID: "user-1", Label: "Laptop",
		PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMzdhbPIA9osmLQz0iTvx/VNJP8fjiD3wfl9LSn2d92", CreatedAt: now,
	}
	tests := []struct {
		name   string
		mutate func(*domain.SSHKeyRegistration)
	}{
		{name: "blank ID", mutate: func(value *domain.SSHKeyRegistration) { value.ID = " " }},
		{name: "padded ID", mutate: func(value *domain.SSHKeyRegistration) { value.ID = " ssh-key-1" }},
		{name: "blank owner", mutate: func(value *domain.SSHKeyRegistration) { value.OwnerUserID = " " }},
		{name: "padded owner", mutate: func(value *domain.SSHKeyRegistration) { value.OwnerUserID = "user-1 " }},
		{name: "blank label", mutate: func(value *domain.SSHKeyRegistration) { value.Label = " " }},
		{name: "long label", mutate: func(value *domain.SSHKeyRegistration) { value.Label = strings.Repeat("a", 81) }},
		{name: "private key body", mutate: func(value *domain.SSHKeyRegistration) { value.PublicKey = "-----BEGIN OPENSSH PRIVATE KEY-----" }},
		{name: "malformed Ed25519 blob", mutate: func(value *domain.SSHKeyRegistration) { value.PublicKey = "ssh-ed25519 AAAA" }},
		{name: "embedded base64 newline", mutate: func(value *domain.SSHKeyRegistration) {
			value.PublicKey = strings.Replace(value.PublicKey, "AAAAC3", "AAAA\nC3", 1)
		}},
		{name: "public key comment", mutate: func(value *domain.SSHKeyRegistration) { value.PublicKey += " laptop@example" }},
		{name: "missing creation", mutate: func(value *domain.SSHKeyRegistration) { value.CreatedAt = time.Time{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if _, err := domain.RegisterSSHKey(candidate); err == nil {
				t.Fatal("RegisterSSHKey() error = nil")
			}
		})
	}
}

func TestRestoreSSHKeyAcceptsValidRevocationAndRejectsInvalidPersistedIdentity(t *testing.T) {
	now := time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC)
	revokedAt := now.Add(time.Minute)
	valid := domain.SSHKeySnapshot{
		ID: "ssh-key-1", OwnerUserID: "user-1", Label: "Laptop", Algorithm: domain.SSHKeyEd25519,
		Fingerprint: "SHA256:mhqmgVD6eE8cj8LlLtVABSgyzWTHFDkHMX7Irr3oI0w",
		PublicKey:   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMzdhbPIA9osmLQz0iTvx/VNJP8fjiD3wfl9LSn2d92",
		CreatedAt:   now, RevokedAt: &revokedAt,
	}
	key, err := domain.RestoreSSHKey(valid)
	if err != nil {
		t.Fatalf("RestoreSSHKey(): %v", err)
	}
	revokedAt = now.Add(2 * time.Minute)
	snapshot := key.Snapshot()
	if snapshot.RevokedAt == nil || !snapshot.RevokedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("restored revocation = %v", snapshot.RevokedAt)
	}
	*snapshot.RevokedAt = now.Add(3 * time.Minute)
	if got := key.Snapshot().RevokedAt; got == nil || !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("Snapshot() leaked revocation pointer: %v", got)
	}

	tests := []struct {
		name   string
		mutate func(*domain.SSHKeySnapshot)
	}{
		{name: "wrong algorithm", mutate: func(value *domain.SSHKeySnapshot) { value.Algorithm = "ssh-rsa" }},
		{name: "bad fingerprint", mutate: func(value *domain.SSHKeySnapshot) { value.Fingerprint = "MD5:aa:bb" }},
		{name: "mismatched fingerprint", mutate: func(value *domain.SSHKeySnapshot) { value.Fingerprint = "SHA256:" + strings.Repeat("B", 43) }},
		{name: "revoked before creation", mutate: func(value *domain.SSHKeySnapshot) { before := now.Add(-time.Minute); value.RevokedAt = &before }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if _, err := domain.RestoreSSHKey(candidate); err == nil {
				t.Fatal("RestoreSSHKey() error = nil")
			}
		})
	}
}
