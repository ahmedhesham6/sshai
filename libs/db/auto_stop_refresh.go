package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

func (store *Store) PendingAutoStopPolicyRefresh(ctx context.Context, environmentID string) (domain.AutoStopPolicyRefresh, bool, error) {
	if strings.TrimSpace(environmentID) == "" || environmentID != strings.TrimSpace(environmentID) {
		return domain.AutoStopPolicyRefresh{}, false, errors.New("load pending Auto-stop Policy refresh: canonical Environment ID is required")
	}
	row, err := store.queries.GetPendingAutoStopPolicyRefresh(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AutoStopPolicyRefresh{}, false, nil
	}
	if err != nil {
		return domain.AutoStopPolicyRefresh{}, false, fmt.Errorf("load pending Auto-stop Policy refresh: %w", err)
	}
	refresh, err := autoStopPolicyRefresh(row.EnvironmentID, row.Generation)
	return refresh, true, err
}

func (store *Store) PendingAutoStopPolicyRefreshes(ctx context.Context, limit int) ([]domain.AutoStopPolicyRefresh, error) {
	if limit < 1 || limit > math.MaxInt32 {
		return nil, errors.New("list pending Auto-stop Policy refreshes: positive 32-bit limit is required")
	}
	rows, err := store.queries.ListPendingAutoStopPolicyRefreshes(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("list pending Auto-stop Policy refreshes: %w", err)
	}
	refreshes := make([]domain.AutoStopPolicyRefresh, 0, len(rows))
	for _, row := range rows {
		refresh, err := autoStopPolicyRefresh(row.EnvironmentID, row.Generation)
		if err != nil {
			return nil, err
		}
		refreshes = append(refreshes, refresh)
	}
	return refreshes, nil
}

func (store *Store) AcknowledgeAutoStopPolicyRefresh(ctx context.Context, environmentID string, generation uint64) error {
	if strings.TrimSpace(environmentID) == "" || environmentID != strings.TrimSpace(environmentID) || generation == 0 || generation > math.MaxInt64 {
		return errors.New("acknowledge Auto-stop Policy refresh: canonical Environment ID and generation are required")
	}
	updated, err := store.queries.AcknowledgeAutoStopPolicyRefresh(ctx, dbsql.AcknowledgeAutoStopPolicyRefreshParams{
		EnvironmentID: environmentID, Generation: int64(generation),
	})
	if err != nil {
		return fmt.Errorf("acknowledge Auto-stop Policy refresh: %w", err)
	}
	if updated == 0 {
		return ErrReferenceNotOwned
	}
	return nil
}

func (store *Store) DeferAutoStopPolicyRefresh(ctx context.Context, environmentID string, generation uint64, attemptedAt time.Time) error {
	if strings.TrimSpace(environmentID) == "" || environmentID != strings.TrimSpace(environmentID) || generation == 0 || generation > math.MaxInt64 || attemptedAt.IsZero() {
		return errors.New("defer Auto-stop Policy refresh: canonical Environment, generation, and attempt time are required")
	}
	if _, err := store.queries.DeferAutoStopPolicyRefresh(ctx, dbsql.DeferAutoStopPolicyRefreshParams{
		EnvironmentID: environmentID, Generation: int64(generation), AttemptedAt: timestamp(attemptedAt),
	}); err != nil {
		return fmt.Errorf("defer Auto-stop Policy refresh: %w", err)
	}
	return nil
}

func autoStopPolicyRefresh(environmentID string, generation int64) (domain.AutoStopPolicyRefresh, error) {
	if environmentID == "" || generation < 1 {
		return domain.AutoStopPolicyRefresh{}, errors.New("restore Auto-stop Policy refresh: database returned invalid identity or generation")
	}
	return domain.AutoStopPolicyRefresh{EnvironmentID: environmentID, Generation: uint64(generation)}, nil
}
