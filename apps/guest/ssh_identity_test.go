package guest_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"golang.org/x/crypto/ssh"
)

type staticHostKeyGenerator struct {
	identity guest.GeneratedSSHHostIdentity
	calls    int
}

func (generator *staticHostKeyGenerator) GenerateEd25519HostIdentity(context.Context) (guest.GeneratedSSHHostIdentity, error) {
	generator.calls++
	return generator.identity, nil
}

func TestReconcileSSHHostIdentityPersistsAndRematerializesWithoutPrivateOutput(t *testing.T) {
	layout := bootstrapGuestState(t)
	sshdRoot := t.TempDir()
	identity := generatedHostIdentity(t)
	generator := &staticHostKeyGenerator{identity: identity}
	request := guest.SSHHostIdentityRequest{PlatformRoot: layout.Platform, SSHDRoot: sshdRoot}

	first, err := guest.ReconcileSSHHostIdentity(t.Context(), request, generator)
	if err != nil {
		t.Fatalf("ReconcileSSHHostIdentity(): %v", err)
	}
	if !first.Generated || !first.Materialized || first.Fingerprint == "" || generator.calls != 1 {
		t.Fatalf("first host identity result/calls = %#v/%d", first, generator.calls)
	}
	privatePath := filepath.Join(layout.Platform, "ssh", "host-ed25519", "private")
	publicPath := filepath.Join(layout.Platform, "ssh", "host-ed25519", "public")
	assertFile(t, privatePath, identity.PrivateKey, 0o600)
	assertFile(t, publicPath, identity.PublicKey, 0o600)
	assertFile(t, filepath.Join(sshdRoot, "ssh_host_ed25519_key"), identity.PrivateKey, 0o600)
	assertFile(t, filepath.Join(sshdRoot, "ssh_host_ed25519_key.pub"), identity.PublicKey, 0o600)

	if err := os.Remove(filepath.Join(sshdRoot, "ssh_host_ed25519_key")); err != nil {
		t.Fatalf("remove materialized private key: %v", err)
	}
	second, err := guest.ReconcileSSHHostIdentity(t.Context(), request, nil)
	if err != nil {
		t.Fatalf("ReconcileSSHHostIdentity() replacement boot: %v", err)
	}
	if second.Generated || !second.Materialized || second.Fingerprint != first.Fingerprint || generator.calls != 1 {
		t.Fatalf("replacement host identity result/calls = %#v/%d", second, generator.calls)
	}
	assertFile(t, filepath.Join(sshdRoot, "ssh_host_ed25519_key"), identity.PrivateKey, 0o600)
}

func TestReconcileSSHHostIdentityRejectsMissingMismatchedAndUnsafeFiles(t *testing.T) {
	t.Run("missing without explicit generator", func(t *testing.T) {
		layout := bootstrapGuestState(t)
		if _, err := guest.ReconcileSSHHostIdentity(t.Context(), guest.SSHHostIdentityRequest{PlatformRoot: layout.Platform, SSHDRoot: t.TempDir()}, nil); err == nil {
			t.Fatal("ReconcileSSHHostIdentity() generated implicitly")
		}
	})
	t.Run("mismatched public key", func(t *testing.T) {
		layout := bootstrapGuestState(t)
		request := guest.SSHHostIdentityRequest{PlatformRoot: layout.Platform, SSHDRoot: t.TempDir()}
		generator := &staticHostKeyGenerator{identity: generatedHostIdentity(t)}
		if _, err := guest.ReconcileSSHHostIdentity(t.Context(), request, generator); err != nil {
			t.Fatalf("initial identity: %v", err)
		}
		other := generatedHostIdentity(t)
		if err := os.WriteFile(filepath.Join(layout.Platform, "ssh", "host-ed25519", "public"), other.PublicKey, 0o600); err != nil {
			t.Fatalf("replace public key: %v", err)
		}
		if _, err := guest.ReconcileSSHHostIdentity(t.Context(), request, nil); err == nil {
			t.Fatal("ReconcileSSHHostIdentity() accepted mismatched key pair")
		}
	})
	t.Run("partial durable identity", func(t *testing.T) {
		layout := bootstrapGuestState(t)
		identityRoot := filepath.Join(layout.Platform, "ssh", "host-ed25519")
		if err := os.MkdirAll(identityRoot, 0o700); err != nil {
			t.Fatalf("create identity root: %v", err)
		}
		if err := os.WriteFile(filepath.Join(identityRoot, "private"), generatedHostIdentity(t).PrivateKey, 0o600); err != nil {
			t.Fatalf("write private key: %v", err)
		}
		if _, err := guest.ReconcileSSHHostIdentity(t.Context(), guest.SSHHostIdentityRequest{PlatformRoot: layout.Platform, SSHDRoot: t.TempDir()}, &staticHostKeyGenerator{identity: generatedHostIdentity(t)}); err == nil {
			t.Fatal("ReconcileSSHHostIdentity() replaced partial identity")
		}
	})
	t.Run("unsafe private permissions", func(t *testing.T) {
		layout := bootstrapGuestState(t)
		request := guest.SSHHostIdentityRequest{PlatformRoot: layout.Platform, SSHDRoot: t.TempDir()}
		if _, err := guest.ReconcileSSHHostIdentity(t.Context(), request, &staticHostKeyGenerator{identity: generatedHostIdentity(t)}); err != nil {
			t.Fatalf("initial identity: %v", err)
		}
		privatePath := filepath.Join(layout.Platform, "ssh", "host-ed25519", "private")
		if err := os.Chmod(privatePath, 0o644); err != nil {
			t.Fatalf("chmod private key: %v", err)
		}
		if _, err := guest.ReconcileSSHHostIdentity(t.Context(), request, nil); err == nil {
			t.Fatal("ReconcileSSHHostIdentity() accepted unsafe private key")
		}
	})
	t.Run("symlink sshd target", func(t *testing.T) {
		layout := bootstrapGuestState(t)
		sshdRoot := t.TempDir()
		if err := os.Symlink(filepath.Join(layout.Home, "unrelated"), filepath.Join(sshdRoot, "ssh_host_ed25519_key")); err != nil {
			t.Fatalf("create sshd symlink: %v", err)
		}
		request := guest.SSHHostIdentityRequest{PlatformRoot: layout.Platform, SSHDRoot: sshdRoot}
		if _, err := guest.ReconcileSSHHostIdentity(t.Context(), request, &staticHostKeyGenerator{identity: generatedHostIdentity(t)}); err == nil {
			t.Fatal("ReconcileSSHHostIdentity() accepted symlink target")
		}
	})
}

func TestReconcileDevAuthorizedKeysWritesOnlyActiveValidatedEd25519Keys(t *testing.T) {
	layout := bootstrapGuestState(t)
	keyA, fingerprintA := authorizedEd25519Key(t)
	keyB, fingerprintB := authorizedEd25519Key(t)
	privateSentinel := filepath.Join(layout.Home, ".ssh", "id_ed25519")
	if err := os.Mkdir(filepath.Dir(privateSentinel), 0o700); err != nil {
		t.Fatalf("create .ssh: %v", err)
	}
	if err := os.WriteFile(privateSentinel, []byte("must-not-read"), 0o000); err != nil {
		t.Fatalf("write private sentinel: %v", err)
	}
	request := guest.AuthorizedKeysRequest{HomeRoot: layout.Home, Keys: []guest.EnvironmentSSHKey{
		{OwnerID: "owner-b", Fingerprint: fingerprintB, PublicKey: keyB, Active: true},
		{OwnerID: "owner-a", Fingerprint: fingerprintA, PublicKey: keyA, Active: true},
		{OwnerID: "owner-inactive", Fingerprint: "not-validated", PublicKey: "private-or-invalid", Active: false},
	}}
	first, err := guest.ReconcileDevAuthorizedKeys(request)
	if err != nil {
		t.Fatalf("ReconcileDevAuthorizedKeys(): %v", err)
	}
	if first.KeyCount != 2 || !first.Changed {
		t.Fatalf("first authorized keys result = %#v", first)
	}
	want := append([]byte(keyA), []byte(keyB)...)
	assertFile(t, filepath.Join(layout.Home, ".ssh", "authorized_keys"), want, 0o600)
	second, err := guest.ReconcileDevAuthorizedKeys(request)
	if err != nil {
		t.Fatalf("ReconcileDevAuthorizedKeys() replay: %v", err)
	}
	if second.KeyCount != 2 || second.Changed {
		t.Fatalf("replayed authorized keys result = %#v", second)
	}
}

func TestReconcileDevAuthorizedKeysRejectsDuplicateMismatchedAndNonEd25519Keys(t *testing.T) {
	key, fingerprint := authorizedEd25519Key(t)
	tests := []struct {
		name string
		keys []guest.EnvironmentSSHKey
	}{
		{name: "duplicate owner fingerprint", keys: []guest.EnvironmentSSHKey{
			{OwnerID: "owner-1", Fingerprint: fingerprint, PublicKey: key, Active: true},
			{OwnerID: "owner-1", Fingerprint: fingerprint, PublicKey: key, Active: true},
		}},
		{name: "mismatched fingerprint", keys: []guest.EnvironmentSSHKey{{OwnerID: "owner-1", Fingerprint: "SHA256:wrong", PublicKey: key, Active: true}}},
		{name: "key options", keys: []guest.EnvironmentSSHKey{{OwnerID: "owner-1", Fingerprint: fingerprint, PublicKey: "command=\"false\" " + key, Active: true}}},
		{name: "non Ed25519", keys: []guest.EnvironmentSSHKey{rsaAuthorizedKey(t)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := bootstrapGuestState(t)
			if _, err := guest.ReconcileDevAuthorizedKeys(guest.AuthorizedKeysRequest{HomeRoot: layout.Home, Keys: test.keys}); err == nil {
				t.Fatal("ReconcileDevAuthorizedKeys() accepted invalid key set")
			}
		})
	}
}

func TestReconcileDevAuthorizedKeysRejectsSymlinkTarget(t *testing.T) {
	layout := bootstrapGuestState(t)
	sshRoot := filepath.Join(layout.Home, ".ssh")
	if err := os.Mkdir(sshRoot, 0o700); err != nil {
		t.Fatalf("create .ssh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sshRoot, "private-sentinel"), []byte("private"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if err := os.Symlink("private-sentinel", filepath.Join(sshRoot, "authorized_keys")); err != nil {
		t.Fatalf("create authorized_keys symlink: %v", err)
	}
	key, fingerprint := authorizedEd25519Key(t)
	_, err := guest.ReconcileDevAuthorizedKeys(guest.AuthorizedKeysRequest{HomeRoot: layout.Home, Keys: []guest.EnvironmentSSHKey{{
		OwnerID: "owner-1", Fingerprint: fingerprint, PublicKey: key, Active: true,
	}}})
	if err == nil {
		t.Fatal("ReconcileDevAuthorizedKeys() accepted symlink target")
	}
}

func bootstrapGuestState(t *testing.T) guest.PersistentStateLayout {
	t.Helper()
	root := t.TempDir()
	layout, err := guest.BootstrapPersistentState(t.Context(), persistentStateRequest(root), staticMountInspector{mount: persistentMount(root)})
	if err != nil {
		t.Fatalf("bootstrap guest state: %v", err)
	}
	return layout
}

func generatedHostIdentity(t *testing.T) guest.GeneratedSSHHostIdentity {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return guest.GeneratedSSHHostIdentity{PrivateKey: pem.EncodeToMemory(block), PublicKey: ssh.MarshalAuthorizedKey(sshPublic)}
}

func authorizedEd25519Key(t *testing.T) (string, string) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return string(ssh.MarshalAuthorizedKey(sshPublic)), ssh.FingerprintSHA256(sshPublic)
}

func rsaAuthorizedKey(t *testing.T) guest.EnvironmentSSHKey {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	public, err := ssh.NewPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatalf("marshal RSA key: %v", err)
	}
	return guest.EnvironmentSSHKey{OwnerID: "owner-1", Fingerprint: ssh.FingerprintSHA256(public), PublicKey: string(ssh.MarshalAuthorizedKey(public)), Active: true}
}

func assertFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("%s content differs", path)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		t.Fatalf("%s mode/type/error = %v/%v", path, info, err)
	}
}
