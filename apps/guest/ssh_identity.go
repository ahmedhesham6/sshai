package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
)

type GeneratedSSHHostIdentity struct {
	PrivateKey []byte
	PublicKey  []byte
}

type SSHHostIdentityGenerator interface {
	GenerateEd25519HostIdentity(context.Context) (GeneratedSSHHostIdentity, error)
}

type SSHHostIdentityRequest struct {
	PlatformRoot string
	SSHDRoot     string
}

type SSHHostIdentityStatus struct {
	Fingerprint  string
	Generated    bool
	Materialized bool
}

func ReconcileSSHHostIdentity(ctx context.Context, request SSHHostIdentityRequest, generator SSHHostIdentityGenerator) (SSHHostIdentityStatus, error) {
	platformPath, err := validatePersistentRoot(request.PlatformRoot)
	if err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: platform root: %w", err)
	}
	sshdPath, err := validateSystemRoot(request.SSHDRoot)
	if err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: sshd root: %w", err)
	}
	platform, err := os.OpenRoot(platformPath)
	if err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: open platform root: %w", err)
	}
	defer platform.Close()
	if err := ensureSecureDirectory(platform, "ssh"); err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: %w", err)
	}
	generated := false
	if _, err := platform.Lstat(filepath.Join("ssh", "host-ed25519")); errors.Is(err, fs.ErrNotExist) {
		if generator == nil {
			return SSHHostIdentityStatus{}, errors.New("reconcile SSH host identity: durable host identity is missing and no generator was supplied")
		}
		identity, err := generator.GenerateEd25519HostIdentity(ctx)
		if err != nil {
			return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: generate identity: %w", err)
		}
		if _, _, err := validateSSHKeyPair(identity.PrivateKey, identity.PublicKey); err != nil {
			return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: generated identity: %w", err)
		}
		if err := persistHostIdentity(platform, identity); err != nil {
			return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: persist identity: %w", err)
		}
		generated = true
	} else if err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: inspect durable identity: %w", err)
	}
	privateKey, publicKey, fingerprint, err := readHostIdentity(platform)
	if err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: %w", err)
	}
	sshd, err := os.OpenRoot(sshdPath)
	if err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: open sshd root: %w", err)
	}
	defer sshd.Close()
	privateChanged, err := replaceAtomicFile(sshd, "ssh_host_ed25519_key", privateKey, 0o600)
	if err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: materialize private key: %w", err)
	}
	publicChanged, err := replaceAtomicFile(sshd, "ssh_host_ed25519_key.pub", publicKey, 0o600)
	if err != nil {
		return SSHHostIdentityStatus{}, fmt.Errorf("reconcile SSH host identity: materialize public key: %w", err)
	}
	return SSHHostIdentityStatus{Fingerprint: fingerprint, Generated: generated, Materialized: privateChanged || publicChanged}, nil
}

func persistHostIdentity(root *os.Root, identity GeneratedSSHHostIdentity) error {
	token := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return err
	}
	temporary := filepath.Join("ssh", ".host-ed25519.tmp-"+hex.EncodeToString(token))
	if err := root.Mkdir(temporary, 0o700); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.RemoveAll(temporary)
		}
	}()
	if err := writeAtomicFile(root, filepath.Join(temporary, "private"), identity.PrivateKey); err != nil {
		return err
	}
	if err := writeAtomicFile(root, filepath.Join(temporary, "public"), identity.PublicKey); err != nil {
		return err
	}
	if err := syncRootDirectory(root, temporary); err != nil {
		return err
	}
	if err := root.Rename(temporary, filepath.Join("ssh", "host-ed25519")); err != nil {
		return err
	}
	cleanup = false
	return syncRootDirectory(root, "ssh")
}

func readHostIdentity(root *os.Root) ([]byte, []byte, string, error) {
	identityRoot := filepath.Join("ssh", "host-ed25519")
	if err := ensureSecureDirectory(root, identityRoot); err != nil {
		return nil, nil, "", err
	}
	directory, err := root.OpenRoot(identityRoot)
	if err != nil {
		return nil, nil, "", err
	}
	defer directory.Close()
	entries, err := fs.ReadDir(directory.FS(), ".")
	if err != nil {
		return nil, nil, "", err
	}
	if len(entries) != 2 || entries[0].Name() != "private" || entries[1].Name() != "public" {
		return nil, nil, "", errors.New("durable host identity must contain exactly private and public key files")
	}
	privateKey, err := readSecureFile(directory, "private")
	if err != nil {
		return nil, nil, "", err
	}
	publicKey, err := readSecureFile(directory, "public")
	if err != nil {
		return nil, nil, "", err
	}
	_, fingerprint, err := validateSSHKeyPair(privateKey, publicKey)
	return privateKey, publicKey, fingerprint, err
}

func validateSSHKeyPair(privateBytes, publicBytes []byte) (ssh.PublicKey, string, error) {
	privateKey, err := ssh.ParseRawPrivateKey(privateBytes)
	if err != nil {
		return nil, "", errors.New("private host key is not valid OpenSSH key material")
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil, "", errors.New("private host key is not Ed25519")
	}
	publicKey, _, options, rest, err := ssh.ParseAuthorizedKey(publicBytes)
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 || publicKey.Type() != ssh.KeyAlgoED25519 {
		return nil, "", errors.New("public host key is not one Ed25519 key")
	}
	if !bytes.Equal(publicKey.Marshal(), signer.PublicKey().Marshal()) {
		return nil, "", errors.New("public host key does not match private host key")
	}
	return publicKey, ssh.FingerprintSHA256(publicKey), nil
}

type EnvironmentSSHKey struct {
	OwnerID     string
	Fingerprint string
	PublicKey   string
	Active      bool
}

type AuthorizedKeysRequest struct {
	HomeRoot string
	Keys     []EnvironmentSSHKey
}

type AuthorizedKeysStatus struct {
	KeyCount int
	Changed  bool
}

func ReconcileDevAuthorizedKeys(request AuthorizedKeysRequest) (AuthorizedKeysStatus, error) {
	homePath, err := validatePersistentRoot(request.HomeRoot)
	if err != nil {
		return AuthorizedKeysStatus{}, fmt.Errorf("reconcile dev authorized keys: home root: %w", err)
	}
	type validatedKey struct {
		owner       string
		fingerprint string
		publicKey   ssh.PublicKey
	}
	validated := make([]validatedKey, 0, len(request.Keys))
	ownerFingerprints := make(map[string]struct{}, len(request.Keys))
	for _, key := range request.Keys {
		if !key.Active {
			continue
		}
		owner := strings.TrimSpace(key.OwnerID)
		if owner == "" || strings.TrimSpace(key.Fingerprint) == "" {
			return AuthorizedKeysStatus{}, errors.New("reconcile dev authorized keys: active key owner and fingerprint are required")
		}
		publicKey, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(key.PublicKey))
		if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 || publicKey.Type() != ssh.KeyAlgoED25519 {
			return AuthorizedKeysStatus{}, errors.New("reconcile dev authorized keys: active key must be one option-free Ed25519 public key")
		}
		fingerprint := ssh.FingerprintSHA256(publicKey)
		if key.Fingerprint != fingerprint {
			return AuthorizedKeysStatus{}, errors.New("reconcile dev authorized keys: active key fingerprint does not match public key")
		}
		ownerFingerprint := owner + "\x00" + fingerprint
		if _, duplicate := ownerFingerprints[ownerFingerprint]; duplicate {
			return AuthorizedKeysStatus{}, errors.New("reconcile dev authorized keys: owner has duplicate key fingerprint")
		}
		ownerFingerprints[ownerFingerprint] = struct{}{}
		validated = append(validated, validatedKey{owner: owner, fingerprint: fingerprint, publicKey: publicKey})
	}
	slices.SortFunc(validated, func(left, right validatedKey) int {
		if compared := strings.Compare(left.owner, right.owner); compared != 0 {
			return compared
		}
		return strings.Compare(left.fingerprint, right.fingerprint)
	})
	seenPublicKeys := make(map[string]struct{}, len(validated))
	content := make([]byte, 0, len(validated)*96)
	for _, key := range validated {
		encoded := string(key.publicKey.Marshal())
		if _, duplicate := seenPublicKeys[encoded]; duplicate {
			continue
		}
		seenPublicKeys[encoded] = struct{}{}
		content = append(content, ssh.MarshalAuthorizedKey(key.publicKey)...)
	}
	home, err := os.OpenRoot(homePath)
	if err != nil {
		return AuthorizedKeysStatus{}, fmt.Errorf("reconcile dev authorized keys: open home root: %w", err)
	}
	defer home.Close()
	if err := ensureSecureDirectory(home, ".ssh"); err != nil {
		return AuthorizedKeysStatus{}, fmt.Errorf("reconcile dev authorized keys: %w", err)
	}
	changed, err := replaceAtomicFile(home, filepath.Join(".ssh", "authorized_keys"), content, 0o600)
	if err != nil {
		return AuthorizedKeysStatus{}, fmt.Errorf("reconcile dev authorized keys: %w", err)
	}
	return AuthorizedKeysStatus{KeyCount: len(seenPublicKeys), Changed: changed}, nil
}

func replaceAtomicFile(root *os.Root, name string, content []byte, mode fs.FileMode) (bool, error) {
	if err := validateRegularFile(root, name, mode); err == nil {
		existing, err := root.ReadFile(name)
		if err != nil {
			return false, err
		}
		if bytes.Equal(existing, content) {
			return false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	token := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return false, err
	}
	temporary := name + ".tmp-" + hex.EncodeToString(token)
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return false, err
	}
	remove := true
	defer func() {
		if remove {
			_ = root.Remove(temporary)
		}
	}()
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := root.Rename(temporary, name); err != nil {
		return false, err
	}
	remove = false
	if err := syncRootDirectory(root, filepath.Dir(name)); err != nil {
		return false, err
	}
	return true, nil
}
