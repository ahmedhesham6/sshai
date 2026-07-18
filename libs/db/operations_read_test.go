package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStoreGetsOwnedOperationWithSteps(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO operation_steps (id, operation_id, step_key, status, attempt, summary, started_at, completed_at)
		VALUES ('step-1', 'operation-1', 'reserve', 'succeeded', 1, 'Reserve Environment', $1, $2)`,
		createdAt, createdAt.Add(time.Second)); err != nil {
		t.Fatalf("insert Operation Step: %v", err)
	}

	detail, err := store.GetOwnedOperation(ctx, "user-1", "operation-1")
	if err != nil {
		t.Fatalf("get owned Operation: %v", err)
	}
	if got := detail.Operation.Snapshot().ID; got != "operation-1" {
		t.Fatalf("Operation ID = %q, want operation-1", got)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].StepKey != "reserve" || detail.Steps[0].Status != "succeeded" || detail.Steps[0].Summary != "Reserve Environment" {
		t.Fatalf("Operation Steps = %#v", detail.Steps)
	}
}

func TestStoreRejectsForeignOrAbsentOwnedOperation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}

	if _, err := store.GetOwnedOperation(ctx, "user-2", "operation-1"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign owner Get error = %v, want ErrReferenceNotOwned", err)
	}
	if _, err := store.GetOwnedOperation(ctx, "user-1", "missing-operation"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("absent Operation Get error = %v, want ErrReferenceNotOwned", err)
	}
}

func TestStoreListsOwnedEnvironmentEventsFromOperations(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}

	events, err := store.ListOwnedEnvironmentEvents(ctx, "user-1", "environment-1")
	if err != nil {
		t.Fatalf("list owned Environment events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Environment event count = %d, want 1", len(events))
	}
	if events[0].ID != "operation-1" || events[0].EnvironmentID != "environment-1" || events[0].OperationID == nil || *events[0].OperationID != "operation-1" {
		t.Fatalf("Environment event = %#v", events[0])
	}
	if events[0].Type != string(domain.OperationEnvironmentCreate) || events[0].Summary == "" {
		t.Fatalf("Environment event type/summary = %#v", events[0])
	}
	if !events[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("Environment event created at = %s, want %s", events[0].CreatedAt, createdAt)
	}
}

func TestStoreRejectsForeignOrAbsentOwnedEnvironmentEvents(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}

	if _, err := store.ListOwnedEnvironmentEvents(ctx, "user-2", "environment-1"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign owner List error = %v, want ErrReferenceNotOwned", err)
	}
	if _, err := store.ListOwnedEnvironmentEvents(ctx, "user-1", "missing-environment"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("absent Environment List error = %v, want ErrReferenceNotOwned", err)
	}
}
