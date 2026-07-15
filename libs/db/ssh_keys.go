package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type sshKeyRegistrationInput struct {
	Action      string                 `json:"action"`
	Label       string                 `json:"label"`
	Algorithm   domain.SSHKeyAlgorithm `json:"algorithm"`
	Fingerprint string                 `json:"fingerprint"`
	PublicKey   string                 `json:"publicKey"`
}

type sshKeyRevocationInput struct {
	Action   string `json:"action"`
	SSHKeyID string `json:"sshKeyId"`
}

func (store *Store) RegisterSSHKey(ctx context.Context, candidate domain.SSHKey, idempotencyKey string) (domain.SSHKey, error) {
	if idempotencyKey == "" || idempotencyKey != strings.TrimSpace(idempotencyKey) {
		return domain.SSHKey{}, errors.New("register SSH Key: canonical idempotency key is required")
	}
	snapshot := candidate.Snapshot()
	input, err := json.Marshal(sshKeyRegistrationInputFromSnapshot(snapshot))
	if err != nil {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: encode input: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	registrationKey := dbsql.LockSSHKeyRegistrationParams{OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey}
	if _, err := queries.LockSSHKeyRegistration(ctx, registrationKey); err != nil {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: lock idempotency key: %w", err)
	}
	existing, err := queries.GetSSHKeyRegistration(ctx, dbsql.GetSSHKeyRegistrationParams(registrationKey))
	if err == nil {
		if !sameJSON(existing.Input, input) {
			return domain.SSHKey{}, ErrIdempotencyConflict
		}
		key, err := restoreSSHKey(existing.ID, existing.OwnerUserID, existing.Label, existing.Algorithm, existing.Fingerprint, existing.PublicKey, existing.CreatedAt, existing.RevokedAt)
		if err != nil {
			return domain.SSHKey{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.SSHKey{}, fmt.Errorf("register SSH Key: commit replay: %w", err)
		}
		return key, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: read idempotency key: %w", err)
	}

	fingerprintKey := dbsql.LockSSHKeyFingerprintParams{OwnerUserID: snapshot.OwnerUserID, Fingerprint: snapshot.Fingerprint}
	if _, err := queries.LockSSHKeyFingerprint(ctx, fingerprintKey); err != nil {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: lock fingerprint: %w", err)
	}
	stored, err := queries.GetOwnedSSHKeyByFingerprint(ctx, dbsql.GetOwnedSSHKeyByFingerprintParams(fingerprintKey))
	key := candidate
	if err == nil {
		if stored.RevokedAt.Valid {
			return domain.SSHKey{}, ErrIdempotencyConflict
		}
		storedInput, marshalErr := json.Marshal(sshKeyRegistrationInput{
			Action: "register", Label: stored.Label, Algorithm: domain.SSHKeyAlgorithm(stored.Algorithm),
			Fingerprint: stored.Fingerprint, PublicKey: stored.PublicKey,
		})
		if marshalErr != nil {
			return domain.SSHKey{}, fmt.Errorf("register SSH Key: encode stored input: %w", marshalErr)
		}
		if !sameJSON(storedInput, input) {
			return domain.SSHKey{}, ErrIdempotencyConflict
		}
		key, err = restoreSSHKey(stored.ID, stored.OwnerUserID, stored.Label, stored.Algorithm, stored.Fingerprint, stored.PublicKey, stored.CreatedAt, stored.RevokedAt)
		if err != nil {
			return domain.SSHKey{}, err
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		if err := queries.InsertSSHKey(ctx, dbsql.InsertSSHKeyParams{
			ID: snapshot.ID, OwnerUserID: snapshot.OwnerUserID, Label: snapshot.Label,
			Algorithm: string(snapshot.Algorithm), Fingerprint: snapshot.Fingerprint, PublicKey: snapshot.PublicKey,
			CreatedAt: timestamp(snapshot.CreatedAt), RevokedAt: optionalTimestamp(snapshot.RevokedAt),
		}); err != nil {
			return domain.SSHKey{}, fmt.Errorf("register SSH Key: insert key: %w", err)
		}
	} else {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: read fingerprint: %w", err)
	}
	if err := queries.InsertSSHKeyRegistration(ctx, dbsql.InsertSSHKeyRegistrationParams{
		OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey,
		Input: input, SshKeyID: key.Snapshot().ID,
	}); err != nil {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: insert idempotency key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.SSHKey{}, fmt.Errorf("register SSH Key: commit: %w", err)
	}
	return key, nil
}

func sshKeyRegistrationInputFromSnapshot(snapshot domain.SSHKeySnapshot) sshKeyRegistrationInput {
	return sshKeyRegistrationInput{
		Action: "register", Label: snapshot.Label, Algorithm: snapshot.Algorithm,
		Fingerprint: snapshot.Fingerprint, PublicKey: snapshot.PublicKey,
	}
}

func (store *Store) RevokeOwnedSSHKey(ctx context.Context, ownerID, sshKeyID, idempotencyKey string, at time.Time) error {
	if ownerID == "" || ownerID != strings.TrimSpace(ownerID) || sshKeyID == "" || sshKeyID != strings.TrimSpace(sshKeyID) ||
		idempotencyKey == "" || idempotencyKey != strings.TrimSpace(idempotencyKey) || at.IsZero() {
		return errors.New("revoke owned SSH Key: canonical owner, key, idempotency, and time are required")
	}
	input, err := json.Marshal(sshKeyRevocationInput{Action: "revoke", SSHKeyID: sshKeyID})
	if err != nil {
		return fmt.Errorf("revoke owned SSH Key: encode input: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("revoke owned SSH Key: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	registrationKey := dbsql.LockSSHKeyRegistrationParams{OwnerUserID: ownerID, IdempotencyKey: idempotencyKey}
	if _, err := queries.LockSSHKeyRegistration(ctx, registrationKey); err != nil {
		return fmt.Errorf("revoke owned SSH Key: lock idempotency key: %w", err)
	}
	existing, err := queries.GetSSHKeyRegistration(ctx, dbsql.GetSSHKeyRegistrationParams(registrationKey))
	if err == nil {
		if !sameJSON(existing.Input, input) {
			return ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("revoke owned SSH Key: commit replay: %w", err)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("revoke owned SSH Key: read idempotency key: %w", err)
	}
	row, err := queries.GetOwnedSSHKeyForUpdate(ctx, dbsql.GetOwnedSSHKeyForUpdateParams{OwnerUserID: ownerID, ID: sshKeyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReferenceNotOwned
	}
	if err != nil {
		return fmt.Errorf("revoke owned SSH Key: lock owned key: %w", err)
	}
	key, err := restoreSSHKey(row.ID, row.OwnerUserID, row.Label, row.Algorithm, row.Fingerprint, row.PublicKey, row.CreatedAt, row.RevokedAt)
	if err != nil {
		return err
	}
	revoked, err := key.Revoke(at)
	if err != nil {
		return fmt.Errorf("revoke owned SSH Key: %w", err)
	}
	revokedAt := revoked.Snapshot().RevokedAt
	updated, err := queries.RevokeOwnedSSHKey(ctx, dbsql.RevokeOwnedSSHKeyParams{
		RevokedAt: timestamp(*revokedAt), OwnerUserID: ownerID, ID: sshKeyID,
	})
	if err != nil {
		return fmt.Errorf("revoke owned SSH Key: update key: %w", err)
	}
	if updated != 1 {
		return ErrReferenceNotOwned
	}
	if err := queries.InsertSSHKeyRegistration(ctx, dbsql.InsertSSHKeyRegistrationParams{
		OwnerUserID: ownerID, IdempotencyKey: idempotencyKey, Input: input, SshKeyID: sshKeyID,
	}); err != nil {
		return fmt.Errorf("revoke owned SSH Key: insert idempotency key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("revoke owned SSH Key: commit: %w", err)
	}
	return nil
}

func (store *Store) ListActiveOwnedSSHKeys(ctx context.Context, ownerID string) ([]domain.SSHKey, error) {
	if ownerID == "" || ownerID != strings.TrimSpace(ownerID) {
		return nil, errors.New("list active owned SSH Keys: canonical owner User ID is required")
	}
	rows, err := store.queries.ListActiveOwnedSSHKeys(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list active owned SSH Keys: %w", err)
	}
	keys := make([]domain.SSHKey, len(rows))
	for index, row := range rows {
		key, err := restoreSSHKey(row.ID, row.OwnerUserID, row.Label, row.Algorithm, row.Fingerprint, row.PublicKey, row.CreatedAt, row.RevokedAt)
		if err != nil {
			return nil, err
		}
		keys[index] = key
	}
	return keys, nil
}

func restoreSSHKey(id, ownerID, label, algorithm, fingerprint, publicKey string, createdAt, revokedAt pgtype.Timestamptz) (domain.SSHKey, error) {
	if !createdAt.Valid {
		return domain.SSHKey{}, errors.New("restore SSH Key: database returned invalid timestamps")
	}
	var revoked *time.Time
	if revokedAt.Valid {
		at := revokedAt.Time
		revoked = &at
	}
	key, err := domain.RestoreSSHKey(domain.SSHKeySnapshot{
		ID: id, OwnerUserID: ownerID, Label: label, Algorithm: domain.SSHKeyAlgorithm(algorithm),
		Fingerprint: fingerprint, PublicKey: publicKey, CreatedAt: createdAt.Time, RevokedAt: revoked,
	})
	if err != nil {
		return domain.SSHKey{}, fmt.Errorf("restore SSH Key: %w", err)
	}
	return key, nil
}
