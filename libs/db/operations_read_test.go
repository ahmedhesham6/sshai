package db_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

	events, nextCursor, err := store.ListOwnedEnvironmentEvents(ctx, "user-1", "environment-1", nil, 0)
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
	if nextCursor != nil {
		t.Fatalf("next cursor = %#v, want nil once every Environment event fits on the page", nextCursor)
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

	if _, _, err := store.ListOwnedEnvironmentEvents(ctx, "user-2", "environment-1", nil, 0); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign owner List error = %v, want ErrReferenceNotOwned", err)
	}
	if _, _, err := store.ListOwnedEnvironmentEvents(ctx, "user-1", "missing-environment", nil, 0); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("absent Environment List error = %v, want ErrReferenceNotOwned", err)
	}
}

// TestStorePaginatesOwnedEnvironmentEventsWithStableKeysetWalk confirms the
// keyset contract Finding 1 requires: paging through Environment events with
// a small page size visits every event exactly once, in creation order,
// with no overlap or gaps, and replaying the same request is stable.
func TestStorePaginatesOwnedEnvironmentEventsWithStableKeysetWalk(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	// operation-1 already exists via the reservation above; insert 4 more
	// Operations against the same Environment to build a 5-event timeline.
	for index := 2; index <= 5; index++ {
		suffix := fmt.Sprintf("%d", index)
		if _, err := pool.Exec(ctx, `
			INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, restate_invocation_id, input, created_at, completed_at)
			VALUES ($1, 'environment-1', 'environment.reconcile', 'succeeded', 'user-1', $2, $3, '{}', $4, $4)`,
			"operation-"+suffix, "reconcile-key-"+suffix, "invocation-"+suffix, createdAt.Add(time.Duration(index-1)*time.Minute)); err != nil {
			t.Fatalf("insert Operation %s: %v", suffix, err)
		}
	}

	var cursor *dbstore.Cursor
	var seen []string
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paginated more than 10 times walking 5 events with page size 2; likely stuck in a loop")
		}
		events, next, err := store.ListOwnedEnvironmentEvents(ctx, "user-1", "environment-1", cursor, 2)
		if err != nil {
			t.Fatalf("list owned Environment events page %d: %v", pages, err)
		}
		for _, event := range events {
			seen = append(seen, event.ID)
		}
		if next == nil {
			break
		}
		cursor = next
	}
	want := []string{"operation-1", "operation-2", "operation-3", "operation-4", "operation-5"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("paginated Environment event IDs = %#v, want %#v", seen, want)
	}

	replay, replayNext, err := store.ListOwnedEnvironmentEvents(ctx, "user-1", "environment-1", nil, 2)
	if err != nil {
		t.Fatalf("replay first page: %v", err)
	}
	if len(replay) != 2 || replay[0].ID != "operation-1" || replay[1].ID != "operation-2" {
		t.Fatalf("replayed first page = %#v", replay)
	}
	if replayNext == nil {
		t.Fatal("replayed first page next cursor = nil, want non-nil (3 events remain)")
	}
}

// TestStorePaginatesOwnedEnvironmentEventsDisambiguatingIdenticalCreatedAt
// confirms the keyset boundary case: events sharing the exact same
// created_at are still split across pages deterministically, ordered and
// disambiguated by id, with neither a skip nor a duplicate at the boundary.
func TestStorePaginatesOwnedEnvironmentEventsDisambiguatingIdenticalCreatedAt(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	insertCreationPrerequisites(t, ctx, pool)
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}
	// operation-1 already shares createdAt; add two more Operations at the
	// exact same created_at so only id can order the three.
	for _, id := range []string{"operation-2", "operation-3"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO operations (id, environment_id, type, status, requested_by_user_id, idempotency_key, restate_invocation_id, input, created_at, completed_at)
			VALUES ($1, 'environment-1', 'environment.reconcile', 'succeeded', 'user-1', $2, $3, '{}', $4, $4)`,
			id, "reconcile-key-"+id, "invocation-"+id, createdAt); err != nil {
			t.Fatalf("insert Operation %s: %v", id, err)
		}
	}

	first, cursor, err := store.ListOwnedEnvironmentEvents(ctx, "user-1", "environment-1", nil, 2)
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if len(first) != 2 || first[0].ID != "operation-1" || first[1].ID != "operation-2" {
		t.Fatalf("first page = %#v, want [operation-1 operation-2] ordered by id under a shared created_at", first)
	}
	if cursor == nil || cursor.ID != "operation-2" {
		t.Fatalf("cursor after first page = %#v, want it to key off operation-2", cursor)
	}

	second, secondCursor, err := store.ListOwnedEnvironmentEvents(ctx, "user-1", "environment-1", cursor, 2)
	if err != nil {
		t.Fatalf("list second page: %v", err)
	}
	if len(second) != 1 || second[0].ID != "operation-3" {
		t.Fatalf("second page = %#v, want exactly [operation-3] (no duplicate, no skip at the boundary)", second)
	}
	if secondCursor != nil {
		t.Fatalf("cursor after final page = %#v, want nil", secondCursor)
	}
}
