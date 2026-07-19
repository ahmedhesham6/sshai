package db

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// PruneProviderResources removes soft-deleted Provider Resource lifecycle
// history older than retainAfter. The query always preserves resources owned
// by an Environment's current Runtime.
func (store *Store) PruneProviderResources(ctx context.Context, retainAfter time.Time) (int64, error) {
	if retainAfter.IsZero() {
		return 0, errors.New("prune Provider Resources: retention boundary is required")
	}
	pruned, err := store.queries.PruneProviderResources(ctx, timestamp(retainAfter))
	if err != nil {
		return 0, fmt.Errorf("prune Provider Resources: %w", err)
	}
	return pruned, nil
}
