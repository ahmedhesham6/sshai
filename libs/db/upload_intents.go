package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type uploadIntentInput struct {
	Kind      domain.UploadKind `json:"kind"`
	Digest    string            `json:"digest"`
	SizeBytes int64             `json:"sizeBytes"`
}

func (store *Store) ReserveUploadIntent(ctx context.Context, candidate domain.UploadIntent, idempotencyKey string) (domain.UploadIntent, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return domain.UploadIntent{}, errors.New("reserve Upload Intent: idempotency key is required")
	}
	snapshot := candidate.Snapshot()
	input, err := json.Marshal(uploadIntentInput{Kind: snapshot.Kind, Digest: snapshot.Digest, SizeBytes: snapshot.SizeBytes})
	if err != nil {
		return domain.UploadIntent{}, fmt.Errorf("reserve Upload Intent: encode input: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.UploadIntent{}, fmt.Errorf("reserve Upload Intent: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	key := dbsql.LockUploadIntentRegistrationParams{OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey}
	if _, err := queries.LockUploadIntentRegistration(ctx, key); err != nil {
		return domain.UploadIntent{}, fmt.Errorf("reserve Upload Intent: lock idempotency key: %w", err)
	}
	existing, err := queries.GetUploadIntentRegistration(ctx, dbsql.GetUploadIntentRegistrationParams(key))
	if err == nil {
		if !sameJSON(existing.Input, input) {
			return domain.UploadIntent{}, ErrIdempotencyConflict
		}
		intent, err := restoreUploadIntent(existing.ID, existing.OwnerUserID, existing.Kind, existing.Digest, existing.SizeBytes, existing.ObjectKey, existing.CreatedAt, existing.ExpiresAt)
		if err != nil {
			return domain.UploadIntent{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.UploadIntent{}, fmt.Errorf("reserve Upload Intent: commit replay: %w", err)
		}
		return intent, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.UploadIntent{}, fmt.Errorf("reserve Upload Intent: read idempotency key: %w", err)
	}
	inserted, err := queries.InsertUploadIntent(ctx, dbsql.InsertUploadIntentParams{
		ID: snapshot.ID, OwnerUserID: snapshot.OwnerUserID, Kind: string(snapshot.Kind), Digest: snapshot.Digest,
		SizeBytes: snapshot.SizeBytes, ObjectKey: snapshot.ObjectKey,
		CreatedAt: timestamp(snapshot.CreatedAt), ExpiresAt: timestamp(snapshot.ExpiresAt),
	})
	if err != nil {
		return domain.UploadIntent{}, fmt.Errorf("reserve Upload Intent: insert intent: %w", err)
	}
	if err := queries.InsertUploadIntentRegistration(ctx, dbsql.InsertUploadIntentRegistrationParams{
		OwnerUserID: snapshot.OwnerUserID, IdempotencyKey: idempotencyKey, Input: input, UploadIntentID: snapshot.ID,
	}); err != nil {
		return domain.UploadIntent{}, fmt.Errorf("reserve Upload Intent: insert idempotency key: %w", err)
	}
	intent, err := restoreUploadIntent(inserted.ID, inserted.OwnerUserID, inserted.Kind, inserted.Digest, inserted.SizeBytes, inserted.ObjectKey, inserted.CreatedAt, inserted.ExpiresAt)
	if err != nil {
		return domain.UploadIntent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.UploadIntent{}, fmt.Errorf("reserve Upload Intent: commit: %w", err)
	}
	return intent, nil
}

func (store *Store) GetOwnedUploadIntentByDigest(ctx context.Context, ownerID string, kind domain.UploadKind, digest string) (domain.UploadIntent, error) {
	row, err := store.queries.GetOwnedUploadIntentByDigest(ctx, dbsql.GetOwnedUploadIntentByDigestParams{
		OwnerUserID: ownerID, Kind: string(kind), Digest: digest,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.UploadIntent{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.UploadIntent{}, fmt.Errorf("get owned Upload Intent: %w", err)
	}
	return restoreUploadIntent(row.ID, row.OwnerUserID, row.Kind, row.Digest, row.SizeBytes, row.ObjectKey, row.CreatedAt, row.ExpiresAt)
}

func restoreUploadIntent(id, ownerID, kind, digest string, sizeBytes int64, objectKey string, createdAt, expiresAt pgtype.Timestamptz) (domain.UploadIntent, error) {
	if !createdAt.Valid || !expiresAt.Valid {
		return domain.UploadIntent{}, errors.New("restore Upload Intent: database returned invalid timestamps")
	}
	intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: id, OwnerUserID: ownerID, Kind: domain.UploadKind(kind), Digest: digest, SizeBytes: sizeBytes,
		ObjectKey: objectKey, CreatedAt: createdAt.Time, ExpiresAt: expiresAt.Time,
	})
	if err != nil {
		return domain.UploadIntent{}, fmt.Errorf("restore Upload Intent: %w", err)
	}
	return intent, nil
}
