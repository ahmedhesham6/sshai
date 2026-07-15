package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"golang.org/x/crypto/ssh"
)

var (
	ErrInvalidSSHKey             = errors.New("invalid SSH Key")
	ErrSSHKeyReservationMismatch = errors.New("SSH Key reservation mismatch")
)

type SSHKeyRepository interface {
	RegisterSSHKey(context.Context, domain.SSHKey, string) (domain.SSHKey, error)
	ListActiveOwnedSSHKeys(context.Context, string) ([]domain.SSHKey, error)
	RevokeOwnedSSHKey(context.Context, string, string, string, time.Time) error
}

type RegisterSSHKeyInput struct {
	OwnerUserID    string
	IdempotencyKey string
	Label          string
	PublicKey      string
}

type RevokeSSHKeyInput struct {
	OwnerUserID    string
	SSHKeyID       string
	IdempotencyKey string
}

type SSHKeyService struct {
	repository SSHKeyRepository
	ids        IDGenerator
	now        func() time.Time
}

func NewSSHKeyService(repository SSHKeyRepository, ids IDGenerator, now func() time.Time) *SSHKeyService {
	return &SSHKeyService{repository: repository, ids: ids, now: now}
}

func (service *SSHKeyService) Register(ctx context.Context, input RegisterSSHKeyInput) (domain.SSHKey, error) {
	if service.repository == nil || service.ids == nil || service.now == nil || !canonicalIdentity(input.OwnerUserID) || !canonicalIdentity(input.IdempotencyKey) || len(input.PublicKey) > 8192 {
		return domain.SSHKey{}, ErrInvalidSSHKey
	}
	publicKey, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(input.PublicKey))
	if err != nil || publicKey.Type() != ssh.KeyAlgoED25519 || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return domain.SSHKey{}, ErrInvalidSSHKey
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))
	candidate, err := domain.RegisterSSHKey(domain.SSHKeyRegistration{
		ID: service.ids.NewID(), OwnerUserID: input.OwnerUserID, Label: input.Label,
		PublicKey: canonical, CreatedAt: service.now(),
	})
	if err != nil {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: %w: %v", ErrInvalidSSHKey, err)
	}
	registered, err := service.repository.RegisterSSHKey(ctx, candidate, input.IdempotencyKey)
	if err != nil {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: persist: %w", err)
	}
	want, got := candidate.Snapshot(), registered.Snapshot()
	if got.OwnerUserID != want.OwnerUserID || got.Label != want.Label || got.Algorithm != want.Algorithm || got.Fingerprint != want.Fingerprint || got.PublicKey != want.PublicKey {
		return domain.SSHKey{}, ErrSSHKeyReservationMismatch
	}
	return registered, nil
}

func (service *SSHKeyService) Revoke(ctx context.Context, input RevokeSSHKeyInput) error {
	if service.repository == nil || service.now == nil || !canonicalIdentity(input.OwnerUserID) || !canonicalIdentity(input.SSHKeyID) || !canonicalIdentity(input.IdempotencyKey) {
		return ErrInvalidSSHKey
	}
	if err := service.repository.RevokeOwnedSSHKey(ctx, input.OwnerUserID, input.SSHKeyID, input.IdempotencyKey, service.now()); err != nil {
		return fmt.Errorf("revoke SSH Key: persist: %w", err)
	}
	return nil
}

func (service *SSHKeyService) List(ctx context.Context, ownerUserID string) ([]domain.SSHKey, error) {
	if service.repository == nil || !canonicalIdentity(ownerUserID) {
		return nil, ErrInvalidSSHKey
	}
	keys, err := service.repository.ListActiveOwnedSSHKeys(ctx, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("list SSH Keys: persist: %w", err)
	}
	result := make([]domain.SSHKey, len(keys))
	for index, key := range keys {
		snapshot := key.Snapshot()
		if snapshot.OwnerUserID != ownerUserID || snapshot.RevokedAt != nil {
			return nil, ErrSSHKeyReservationMismatch
		}
		result[index] = key
	}
	return result, nil
}
