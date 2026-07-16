package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStoreReservesAndAttachesInitialRuntimeAtomically(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	if _, err := store.InventoryEnvironmentState(ctx, "operation-1", environmentStateReservation(createdAt.Add(30*time.Second), "volume-1")); err != nil {
		t.Fatalf("inventory Environment State: %v", err)
	}
	reservation := domain.RuntimeReservation{
		ID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1,
		RuntimePreset: "standard", Region: "us-east-1", AvailabilityZone: "us-east-1a",
		ImageVersion: "image-v1", CreatedAt: createdAt.Add(time.Minute),
	}

	runtime, err := store.ReserveInitialRuntime(ctx, "operation-1", reservation)
	if err != nil {
		t.Fatalf("ReserveInitialRuntime(): %v", err)
	}
	if got := runtime.Snapshot(); got.ID != "runtime-1" || got.Status != domain.RuntimeAbsent || got.Version != 1 {
		t.Fatalf("reserved Runtime = %#v", got)
	}
	var currentRuntimeID, status string
	var environmentVersion int64
	if err := pool.QueryRow(ctx, `
		SELECT environment.current_runtime_id, runtime.status, environment.version
		FROM environments environment
		JOIN runtimes runtime ON runtime.id = environment.current_runtime_id
		WHERE environment.id = 'environment-1'`).Scan(&currentRuntimeID, &status, &environmentVersion); err != nil {
		t.Fatalf("read attached Runtime: %v", err)
	}
	if currentRuntimeID != "runtime-1" || status != "absent" || environmentVersion != 2 {
		t.Fatalf("attached Runtime = %q/%q Environment version %d", currentRuntimeID, status, environmentVersion)
	}
	replayed, err := store.ReserveInitialRuntime(ctx, "operation-1", reservation)
	if err != nil || replayed.Snapshot().ID != runtime.Snapshot().ID {
		t.Fatalf("replayed Runtime = %#v error:%v", replayed.Snapshot(), err)
	}
	var runtimeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtimes WHERE environment_id = 'environment-1'`).Scan(&runtimeCount); err != nil {
		t.Fatalf("count Runtimes: %v", err)
	}
	if runtimeCount != 1 {
		t.Fatalf("Runtime rows after replay = %d", runtimeCount)
	}
}

func TestStoreRejectsInitialRuntimeBeforeStateInventory(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	if _, err := store.ReserveEnvironmentCreation(ctx, newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	reservation := initialRuntimeReservation(createdAt.Add(time.Minute))

	_, err := store.ReserveInitialRuntime(ctx, "operation-1", reservation)
	if !errors.Is(err, dbstore.ErrEnvironmentStateRequired) {
		t.Fatalf("ReserveInitialRuntime() error = %v", err)
	}
	requirePermanentRepositoryError(t, err)
	var runtimes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtimes`).Scan(&runtimes); err != nil || runtimes != 0 {
		t.Fatalf("Runtime rows without State = %d error:%v", runtimes, err)
	}
}

func initialRuntimeReservation(createdAt time.Time) domain.RuntimeReservation {
	return domain.RuntimeReservation{
		ID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1,
		RuntimePreset: "standard", Region: "us-east-1", AvailabilityZone: "us-east-1a",
		ImageVersion: "image-v1", CreatedAt: createdAt,
	}
}
