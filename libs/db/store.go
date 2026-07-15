package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool    *pgxpool.Pool
	queries *dbsql.Queries
}

type EnsureUserInput struct {
	ID            string
	WorkOSUserID  string
	DefaultRegion string
	ObservedAt    time.Time
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, queries: dbsql.New(pool)}
}

func (store *Store) EnsureUser(ctx context.Context, input EnsureUserInput) (domain.User, error) {
	user, err := store.queries.EnsureUser(ctx, dbsql.EnsureUserParams{
		ID:            input.ID,
		WorkosUserID:  input.WorkOSUserID,
		DefaultRegion: input.DefaultRegion,
		ObservedAt:    pgtype.Timestamptz{Time: input.ObservedAt, Valid: true},
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("ensure User: %w", err)
	}
	if !user.CreatedAt.Valid || !user.UpdatedAt.Valid {
		return domain.User{}, errors.New("ensure User: database returned invalid timestamps")
	}
	return domain.User{
		ID:            user.ID,
		WorkOSUserID:  user.WorkosUserID,
		DefaultRegion: user.DefaultRegion,
		CreatedAt:     user.CreatedAt.Time,
		UpdatedAt:     user.UpdatedAt.Time,
	}, nil
}
