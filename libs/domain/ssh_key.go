package domain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type SSHKeyAlgorithm string

const SSHKeyEd25519 SSHKeyAlgorithm = "ssh-ed25519"

var (
	sshFingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

type SSHKeySnapshot struct {
	ID          string
	OwnerUserID string
	Label       string
	Algorithm   SSHKeyAlgorithm
	Fingerprint string
	PublicKey   string
	CreatedAt   time.Time
	RevokedAt   *time.Time
}

type SSHKeyRegistration struct {
	ID          string
	OwnerUserID string
	Label       string
	PublicKey   string
	CreatedAt   time.Time
}

type SSHKey struct {
	snapshot SSHKeySnapshot
}

func RegisterSSHKey(registration SSHKeyRegistration) (SSHKey, error) {
	fingerprint, err := validateSSHKeyIdentity(registration.ID, registration.OwnerUserID, registration.Label, registration.PublicKey, registration.CreatedAt)
	if err != nil {
		return SSHKey{}, fmt.Errorf("register SSH Key: %w", err)
	}
	createdAt := registration.CreatedAt.Round(0).UTC()
	return SSHKey{snapshot: SSHKeySnapshot{
		ID: registration.ID, OwnerUserID: registration.OwnerUserID, Label: registration.Label,
		Algorithm: SSHKeyEd25519, Fingerprint: fingerprint, PublicKey: registration.PublicKey,
		CreatedAt: createdAt,
	}}, nil
}

func RestoreSSHKey(snapshot SSHKeySnapshot) (SSHKey, error) {
	fingerprint, err := validateSSHKeyIdentity(snapshot.ID, snapshot.OwnerUserID, snapshot.Label, snapshot.PublicKey, snapshot.CreatedAt)
	if err != nil {
		return SSHKey{}, fmt.Errorf("restore SSH Key: %w", err)
	}
	if snapshot.Algorithm != SSHKeyEd25519 || !sshFingerprintPattern.MatchString(snapshot.Fingerprint) || snapshot.Fingerprint != fingerprint {
		return SSHKey{}, errors.New("restore SSH Key: canonical Ed25519 public identity is required")
	}
	snapshot.CreatedAt = snapshot.CreatedAt.Round(0).UTC()
	snapshot.RevokedAt = cloneCanonicalTime(snapshot.RevokedAt)
	if snapshot.RevokedAt != nil && snapshot.RevokedAt.Before(snapshot.CreatedAt) {
		return SSHKey{}, errors.New("restore SSH Key: revocation time precedes creation")
	}
	return SSHKey{snapshot: snapshot}, nil
}

func validateSSHKeyIdentity(id, ownerUserID, label, publicKey string, createdAt time.Time) (string, error) {
	if id == "" || id != strings.TrimSpace(id) || ownerUserID == "" || ownerUserID != strings.TrimSpace(ownerUserID) {
		return "", errors.New("ID and owner User ID are required without surrounding whitespace")
	}
	if label != strings.TrimSpace(label) || label == "" || utf8.RuneCountInString(label) > 80 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return "", errors.New("label is invalid")
	}
	if createdAt.IsZero() {
		return "", errors.New("creation time is required")
	}
	fingerprint, ok := ed25519Fingerprint(publicKey)
	if !ok {
		return "", errors.New("canonical Ed25519 public identity is required")
	}
	return fingerprint, nil
}

func ed25519Fingerprint(publicKey string) (string, bool) {
	if len(publicKey) > 8192 {
		return "", false
	}
	parts := strings.Split(publicKey, " ")
	if len(parts) != 2 || parts[0] != string(SSHKeyEd25519) {
		return "", false
	}
	blob, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || base64.StdEncoding.EncodeToString(blob) != parts[1] {
		return "", false
	}
	algorithm, remainder, ok := sshWireString(blob)
	if !ok || string(algorithm) != string(SSHKeyEd25519) {
		return "", false
	}
	key, remainder, ok := sshWireString(remainder)
	if !ok || len(key) != ed25519.PublicKeySize || len(remainder) != 0 {
		return "", false
	}
	digest := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:]), true
}

func sshWireString(value []byte) ([]byte, []byte, bool) {
	if len(value) < 4 {
		return nil, nil, false
	}
	size := uint64(binary.BigEndian.Uint32(value[:4]))
	if size > uint64(len(value)-4) {
		return nil, nil, false
	}
	end := 4 + int(size)
	return value[4:end], value[end:], true
}

func (key SSHKey) Snapshot() SSHKeySnapshot {
	snapshot := key.snapshot
	snapshot.RevokedAt = cloneCanonicalTime(snapshot.RevokedAt)
	return snapshot
}

func (key SSHKey) Revoke(at time.Time) (SSHKey, error) {
	if key.snapshot.RevokedAt != nil {
		return key, nil
	}
	if at.IsZero() || at.Before(key.snapshot.CreatedAt) {
		return SSHKey{}, errors.New("revoke SSH Key: revocation time is invalid")
	}
	next := key.Snapshot()
	at = at.Round(0).UTC()
	next.RevokedAt = &at
	return SSHKey{snapshot: next}, nil
}
