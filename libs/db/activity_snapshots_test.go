package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStorePersistsAndLoadsLatestAutoStopActivitySnapshot(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt)

	first := domain.AutoStopActivitySnapshot{RuntimeID: "runtime-1", Sequence: 1, ObservedAt: createdAt.Add(3 * time.Minute)}
	second := domain.AutoStopActivitySnapshot{
		RuntimeID: "runtime-1", Sequence: 2, ObservedAt: createdAt.Add(4 * time.Minute),
		SSHConnections: 1, IDEConnections: 2, CodexProcesses: 3, ClaudeProcesses: 4,
		ProtectedProcesses: 5, SelectedContainers: 6, UnknownUserProcesses: 7,
	}
	for _, snapshot := range []domain.AutoStopActivitySnapshot{first, second, second} {
		if err := store.StoreActivitySnapshot(ctx, "environment-1", snapshot); err != nil {
			t.Fatalf("StoreActivitySnapshot(%d): %v", snapshot.Sequence, err)
		}
	}

	state, err := store.LatestAutoStopSnapshot(ctx, "environment-1", "runtime-1")
	if err != nil {
		t.Fatal(err)
	}
	if state.RuntimeID != "runtime-1" || state.Policy.ID != "policy-1" || state.PolicyGeneration != 1 || state.Snapshot == nil ||
		state.Snapshot.Sequence != second.Sequence || !state.Snapshot.ObservedAt.Equal(second.ObservedAt) || state.Snapshot.UnknownUserProcesses != second.UnknownUserProcesses {
		t.Fatalf("latest Auto-stop state = %#v", state)
	}
	conflict := second
	conflict.SSHConnections++
	if err := store.StoreActivitySnapshot(ctx, "environment-1", conflict); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting Snapshot replay error = %v", err)
	}
	if err := store.StoreActivitySnapshot(ctx, "environment-foreign", second); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign Environment Snapshot replay error = %v", err)
	}
	if err := store.StoreActivitySnapshot(ctx, "environment-1", domain.AutoStopActivitySnapshot{
		RuntimeID: "runtime-old", Sequence: 1, ObservedAt: createdAt,
	}); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign Runtime Snapshot error = %v", err)
	}
}
