package domain_test

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestOperationCreateLifecycleIsReplaySafe(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID:                "operation-1",
		EnvironmentID:     "environment-1",
		Type:              domain.OperationEnvironmentCreate,
		RequestedByUserID: "user-1",
		IdempotencyKey:    "request-1",
		Input:             []byte(`{"name":"workspace"}`),
		CreatedAt:         createdAt,
	})
	if err != nil {
		t.Fatalf("queue Operation: %v", err)
	}
	if got := operation.Snapshot().Status; got != domain.OperationQueued {
		t.Fatalf("queued status = %q, want %q", got, domain.OperationQueued)
	}

	startedAt := createdAt.Add(time.Second)
	operation, err = operation.Start(startedAt)
	if err != nil {
		t.Fatalf("start Operation: %v", err)
	}
	operation, err = operation.RecordRestateInvocation("invocation-1")
	if err != nil {
		t.Fatalf("record Restate invocation: %v", err)
	}
	replayedInvocation, err := operation.RecordRestateInvocation("invocation-1")
	if err != nil || replayedInvocation.Snapshot().RestateInvocationID == nil || *replayedInvocation.Snapshot().RestateInvocationID != "invocation-1" {
		t.Fatalf("replay Restate invocation = %#v, %v", replayedInvocation.Snapshot(), err)
	}
	if _, err := operation.RecordRestateInvocation("invocation-2"); err == nil {
		t.Fatal("conflicting Restate invocation error = nil")
	}
	replayedStart, err := operation.Start(startedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("replay start Operation: %v", err)
	}
	if got, want := replayedStart.Snapshot(), operation.Snapshot(); got.Status != want.Status || got.CompletedAt != nil {
		t.Fatalf("replayed start changed Operation: got %#v, want %#v", got, want)
	}

	completedAt := startedAt.Add(2 * time.Second)
	operation, err = operation.Succeed(completedAt)
	if err != nil {
		t.Fatalf("succeed Operation: %v", err)
	}
	replayedSuccess, err := operation.Succeed(completedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("replay succeed Operation: %v", err)
	}
	if got, want := replayedSuccess.Snapshot(), operation.Snapshot(); got.Status != want.Status || !got.CompletedAt.Equal(*want.CompletedAt) {
		t.Fatalf("replayed success changed Operation: got %#v, want %#v", got, want)
	}
}

func TestQueueOperationRejectsIncompleteRequests(t *testing.T) {
	valid := domain.OperationRequest{
		ID:                "operation-1",
		EnvironmentID:     "environment-1",
		Type:              domain.OperationEnvironmentCreate,
		RequestedByUserID: "user-1",
		IdempotencyKey:    "request-1",
		Input:             []byte(`{}`),
		CreatedAt:         time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name   string
		mutate func(*domain.OperationRequest)
	}{
		{name: "ID", mutate: func(input *domain.OperationRequest) { input.ID = "" }},
		{name: "Environment ID", mutate: func(input *domain.OperationRequest) { input.EnvironmentID = "" }},
		{name: "type", mutate: func(input *domain.OperationRequest) { input.Type = "" }},
		{name: "requesting User", mutate: func(input *domain.OperationRequest) { input.RequestedByUserID = "" }},
		{name: "idempotency key", mutate: func(input *domain.OperationRequest) { input.IdempotencyKey = "" }},
		{name: "input", mutate: func(input *domain.OperationRequest) { input.Input = nil }},
		{name: "creation time", mutate: func(input *domain.OperationRequest) { input.CreatedAt = time.Time{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.mutate(&input)
			if _, err := domain.QueueOperation(input); err == nil {
				t.Fatal("QueueOperation() error = nil")
			}
		})
	}
}

func TestOperationRejectsOutOfOrderTransitions(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID:                "operation-1",
		EnvironmentID:     "environment-1",
		Type:              domain.OperationEnvironmentCreate,
		RequestedByUserID: "user-1",
		IdempotencyKey:    "request-1",
		Input:             []byte(`{}`),
		CreatedAt:         createdAt,
	})
	if err != nil {
		t.Fatalf("queue Operation: %v", err)
	}
	if _, err := operation.Succeed(createdAt.Add(time.Second)); err == nil {
		t.Fatal("succeed queued Operation error = nil")
	}
	if _, err := operation.Start(createdAt.Add(-time.Second)); err == nil {
		t.Fatal("start before creation error = nil")
	}
	running, err := operation.Start(createdAt.Add(time.Second))
	if err != nil {
		t.Fatalf("start Operation: %v", err)
	}
	if _, err := running.Succeed(createdAt.Add(2 * time.Second)); err == nil {
		t.Fatal("succeed Operation without Restate invocation error = nil")
	}
}
