package domain

import (
	"errors"
	"fmt"
	"time"
)

const SystemIdempotencyKeyPrefix = "system:"

type OperationType string

const (
	OperationEnvironmentCreate         OperationType = "environment.create"
	OperationEnvironmentUpdateAutoStop OperationType = "environment.update_auto_stop"
	OperationRuntimeStart              OperationType = "runtime.start"
	OperationRuntimeStop               OperationType = "runtime.stop"
	OperationRuntimeReplace            OperationType = "runtime.replace"
	OperationProfileResolve            OperationType = "profile.resolve"
)

type OperationStatus string

const (
	OperationQueued    OperationStatus = "queued"
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
	OperationCancelled OperationStatus = "cancelled"
	OperationBlocked   OperationStatus = "blocked"
)

type OperationRequest struct {
	ID                string
	EnvironmentID     string
	Type              OperationType
	RequestedByUserID string
	IdempotencyKey    string
	Input             []byte
	CreatedAt         time.Time
}

type OperationSnapshot struct {
	ID                  string
	EnvironmentID       string
	Type                OperationType
	Status              OperationStatus
	RequestedByUserID   string
	IdempotencyKey      string
	RestateInvocationID *string
	Input               []byte
	CreatedAt           time.Time
	CompletedAt         *time.Time
}

type Operation struct {
	snapshot OperationSnapshot
}

func QueueOperation(request OperationRequest) (Operation, error) {
	required := []struct {
		name  string
		value string
	}{
		{name: "ID", value: request.ID},
		{name: "Environment ID", value: request.EnvironmentID},
		{name: "type", value: string(request.Type)},
		{name: "requesting User ID", value: request.RequestedByUserID},
		{name: "idempotency key", value: request.IdempotencyKey},
	}
	for _, field := range required {
		if field.value == "" {
			return Operation{}, fmt.Errorf("queue Operation: %s is required", field.name)
		}
	}
	if len(request.Input) == 0 {
		return Operation{}, errors.New("queue Operation: input is required")
	}
	if request.CreatedAt.IsZero() {
		return Operation{}, errors.New("queue Operation: creation time is required")
	}

	return Operation{snapshot: OperationSnapshot{
		ID:                request.ID,
		EnvironmentID:     request.EnvironmentID,
		Type:              request.Type,
		Status:            OperationQueued,
		RequestedByUserID: request.RequestedByUserID,
		IdempotencyKey:    request.IdempotencyKey,
		Input:             append([]byte(nil), request.Input...),
		CreatedAt:         request.CreatedAt,
	}}, nil
}

func RestoreOperation(snapshot OperationSnapshot) (Operation, error) {
	if snapshot.Status != OperationQueued && snapshot.Status != OperationRunning && !snapshot.Status.terminal() {
		return Operation{}, fmt.Errorf("restore Operation: unknown status %q", snapshot.Status)
	}
	if snapshot.Status.terminal() && snapshot.CompletedAt == nil {
		return Operation{}, errors.New("restore Operation: terminal Operation requires completion time")
	}
	if snapshot.Status == OperationSucceeded && snapshot.RestateInvocationID == nil && snapshot.Type != OperationEnvironmentUpdateAutoStop {
		return Operation{}, errors.New("restore Operation: succeeded Operation requires Restate invocation")
	}
	if !snapshot.Status.terminal() && snapshot.CompletedAt != nil {
		return Operation{}, errors.New("restore Operation: incomplete Operation cannot have completion time")
	}
	queued, err := QueueOperation(OperationRequest{
		ID:                snapshot.ID,
		EnvironmentID:     snapshot.EnvironmentID,
		Type:              snapshot.Type,
		RequestedByUserID: snapshot.RequestedByUserID,
		IdempotencyKey:    snapshot.IdempotencyKey,
		Input:             snapshot.Input,
		CreatedAt:         snapshot.CreatedAt,
	})
	if err != nil {
		return Operation{}, fmt.Errorf("restore Operation: %w", err)
	}
	queued.snapshot.Status = snapshot.Status
	if snapshot.RestateInvocationID != nil {
		if *snapshot.RestateInvocationID == "" {
			return Operation{}, errors.New("restore Operation: Restate invocation ID cannot be empty")
		}
		invocationID := *snapshot.RestateInvocationID
		queued.snapshot.RestateInvocationID = &invocationID
	}
	if snapshot.CompletedAt != nil {
		completedAt := *snapshot.CompletedAt
		if completedAt.Before(snapshot.CreatedAt) {
			return Operation{}, errors.New("restore Operation: completion time precedes creation time")
		}
		queued.snapshot.CompletedAt = &completedAt
	}
	return queued, nil
}

func (status OperationStatus) terminal() bool {
	return status == OperationSucceeded || status == OperationFailed || status == OperationCancelled || status == OperationBlocked
}

func (operation Operation) Snapshot() OperationSnapshot {
	snapshot := operation.snapshot
	snapshot.Input = append([]byte(nil), operation.snapshot.Input...)
	if operation.snapshot.RestateInvocationID != nil {
		invocationID := *operation.snapshot.RestateInvocationID
		snapshot.RestateInvocationID = &invocationID
	}
	if operation.snapshot.CompletedAt != nil {
		completedAt := *operation.snapshot.CompletedAt
		snapshot.CompletedAt = &completedAt
	}
	return snapshot
}

func (operation Operation) RecordRestateInvocation(invocationID string) (Operation, error) {
	if invocationID == "" {
		return Operation{}, errors.New("record Restate invocation: invocation ID is required")
	}
	if operation.snapshot.RestateInvocationID != nil {
		if *operation.snapshot.RestateInvocationID == invocationID {
			return operation, nil
		}
		return Operation{}, errors.New("record Restate invocation: Operation already belongs to another invocation")
	}
	next := operation.Snapshot()
	next.RestateInvocationID = &invocationID
	return Operation{snapshot: next}, nil
}

func (operation Operation) Start(at time.Time) (Operation, error) {
	if operation.snapshot.Status == OperationRunning {
		return operation, nil
	}
	if operation.snapshot.Status != OperationQueued {
		return Operation{}, fmt.Errorf("start Operation: status is %q, want %q", operation.snapshot.Status, OperationQueued)
	}
	if at.Before(operation.snapshot.CreatedAt) {
		return Operation{}, errors.New("start Operation: start time precedes creation time")
	}

	next := operation.Snapshot()
	next.Status = OperationRunning
	return Operation{snapshot: next}, nil
}

func (operation Operation) Succeed(at time.Time) (Operation, error) {
	if operation.snapshot.Status == OperationSucceeded {
		return operation, nil
	}
	if operation.snapshot.Status != OperationRunning {
		return Operation{}, fmt.Errorf("succeed Operation: status is %q, want %q", operation.snapshot.Status, OperationRunning)
	}
	if operation.snapshot.RestateInvocationID == nil {
		return Operation{}, errors.New("succeed Operation: Restate invocation is required")
	}
	if at.Before(operation.snapshot.CreatedAt) {
		return Operation{}, errors.New("succeed Operation: completion time precedes creation time")
	}

	next := operation.Snapshot()
	next.Status = OperationSucceeded
	next.CompletedAt = &at
	return Operation{snapshot: next}, nil
}

// SucceedSynchronously records the one synchronous command currently
// supported by the control plane without fabricating workflow provenance.
func (operation Operation) SucceedSynchronously(at time.Time) (Operation, error) {
	if operation.snapshot.Status == OperationSucceeded && operation.snapshot.Type == OperationEnvironmentUpdateAutoStop && operation.snapshot.RestateInvocationID == nil {
		return operation, nil
	}
	if operation.snapshot.Type != OperationEnvironmentUpdateAutoStop {
		return Operation{}, fmt.Errorf("succeed Operation synchronously: type %q requires a workflow", operation.snapshot.Type)
	}
	if operation.snapshot.Status != OperationQueued {
		return Operation{}, fmt.Errorf("succeed Operation synchronously: status is %q, want %q", operation.snapshot.Status, OperationQueued)
	}
	if operation.snapshot.RestateInvocationID != nil {
		return Operation{}, errors.New("succeed Operation synchronously: Restate invocation must be absent")
	}
	if at.Before(operation.snapshot.CreatedAt) {
		return Operation{}, errors.New("succeed Operation synchronously: completion time precedes creation time")
	}

	next := operation.Snapshot()
	next.Status = OperationSucceeded
	next.CompletedAt = &at
	return Operation{snapshot: next}, nil
}
