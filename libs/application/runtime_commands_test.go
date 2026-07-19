package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestRuntimeCommandServiceReservesStartBeforeDispatch(t *testing.T) {
	now := time.Date(2026, time.July, 13, 15, 0, 0, 0, time.UTC)
	environment, runtime := stoppedEnvironmentRuntime(t, now.Add(-time.Hour))
	repository := &runtimeCommandRepositoryFake{environment: environment, runtime: runtime}
	dispatcher := &runtimeCommandDispatcherFake{}
	service := application.NewRuntimeCommandService(repository, dispatcher, &idsFake{values: []string{"operation-1"}}, func() time.Time { return now })

	command, err := service.StartRuntime(t.Context(), application.RuntimeCommandInput{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", IdempotencyKey: "request-1",
	})
	if err != nil {
		t.Fatalf("StartRuntime(): %v", err)
	}

	operation := command.Operation().Snapshot()
	if operation.ID != "operation-1" || operation.Type != domain.OperationRuntimeStart || operation.Status != domain.OperationQueued {
		t.Fatalf("reserved Operation = %#v", operation)
	}
	if string(operation.Input) != `{}` || !operation.CreatedAt.Equal(now) {
		t.Fatalf("Operation input/time = %s, %s", operation.Input, operation.CreatedAt)
	}
	if repository.calls != 1 || len(dispatcher.operationIDs) != 1 || dispatcher.operationIDs[0] != "operation-1" {
		t.Fatalf("repository calls = %d, dispatches = %#v", repository.calls, dispatcher.operationIDs)
	}
}

func TestRuntimeCommandServicePersistsAutoStopAuditEvidence(t *testing.T) {
	now := time.Date(2026, time.July, 13, 15, 0, 0, 0, time.UTC)
	environment, runtime := stoppedEnvironmentRuntime(t, now.Add(-time.Hour))
	repository := &runtimeCommandRepositoryFake{environment: environment, runtime: runtime}
	service := application.NewRuntimeCommandService(repository, &runtimeCommandDispatcherFake{}, &idsFake{values: []string{"operation-1"}}, func() time.Time { return now })
	audit := &domain.RuntimeStopAuditEvidence{
		Policy:           domain.AutoStopPolicySnapshot{ID: "policy-1", EnvironmentID: "environment-1", Mode: domain.AutoStopWhenFullyIdle, GracePeriodSeconds: 60},
		PolicyGeneration: 2, GraceStartedAt: now.Add(-time.Minute), GraceExpiredAt: now, GracePeriodSeconds: 60,
		QualifyingSnapshots: []domain.AutoStopActivitySnapshot{
			{RuntimeID: "runtime-1", Sequence: 8, ObservedAt: now.Add(-time.Minute)},
			{RuntimeID: "runtime-1", Sequence: 9, ObservedAt: now},
		},
	}
	command, err := service.StopRuntimeWithReason(t.Context(), application.RuntimeCommandInput{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", IdempotencyKey: "auto-stop-1",
	}, domain.RuntimeStopAutoStop, audit)
	if err != nil {
		t.Fatalf("StopRuntimeWithReason(): %v", err)
	}
	var persisted struct {
		Reason domain.RuntimeStopReason         `json:"reason"`
		Audit  *domain.RuntimeStopAuditEvidence `json:"audit"`
	}
	if err := json.Unmarshal(command.Operation().Snapshot().Input, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Reason != domain.RuntimeStopAutoStop || persisted.Audit == nil || persisted.Audit.PolicyGeneration != 2 || len(persisted.Audit.QualifyingSnapshots) != 2 {
		t.Fatalf("persisted Auto-stop input = %#v", persisted)
	}
}

func TestRuntimeCommandServiceRejectsInvalidInputBeforeSideEffects(t *testing.T) {
	now := time.Date(2026, time.July, 13, 15, 0, 0, 0, time.UTC)
	environment, runtime := stoppedEnvironmentRuntime(t, now.Add(-time.Hour))
	tests := []application.RuntimeCommandInput{
		{OwnerUserID: " user-1", EnvironmentID: "environment-1", IdempotencyKey: "request-1"},
		{OwnerUserID: "user-1", EnvironmentID: "environment-1 ", IdempotencyKey: "request-1"},
		{OwnerUserID: "user-1", EnvironmentID: "environment-1", IdempotencyKey: "request-1 "},
	}
	for _, input := range tests {
		repository := &runtimeCommandRepositoryFake{environment: environment, runtime: runtime}
		dispatcher := &runtimeCommandDispatcherFake{}
		service := application.NewRuntimeCommandService(repository, dispatcher, &idsFake{values: []string{"operation-1"}}, func() time.Time { return now })
		if _, err := service.StartRuntime(t.Context(), input); !errors.Is(err, application.ErrInvalidRuntimeCommand) || repository.calls != 0 || len(dispatcher.operationIDs) != 0 {
			t.Fatalf("StartRuntime(%#v) = repository:%d dispatch:%d error:%v", input, repository.calls, len(dispatcher.operationIDs), err)
		}
	}
}

func TestRuntimeCommandServiceRejectsIncompleteDependenciesWithoutPanic(t *testing.T) {
	now := time.Date(2026, time.July, 13, 15, 0, 0, 0, time.UTC)
	environment, runtime := stoppedEnvironmentRuntime(t, now.Add(-time.Hour))
	repository := &runtimeCommandRepositoryFake{environment: environment, runtime: runtime}
	dispatcher := &runtimeCommandDispatcherFake{}
	ids := &idsFake{values: []string{"operation-1"}}
	input := application.RuntimeCommandInput{OwnerUserID: "user-1", EnvironmentID: "environment-1", IdempotencyKey: "request-1"}
	services := []*application.RuntimeCommandService{
		application.NewRuntimeCommandService(nil, dispatcher, ids, func() time.Time { return now }),
		application.NewRuntimeCommandService(repository, nil, ids, func() time.Time { return now }),
		application.NewRuntimeCommandService(repository, dispatcher, nil, func() time.Time { return now }),
		application.NewRuntimeCommandService(repository, dispatcher, ids, nil),
	}
	for index, service := range services {
		if _, err := service.StartRuntime(t.Context(), input); !errors.Is(err, application.ErrInvalidRuntimeCommand) {
			t.Fatalf("service %d error = %v", index, err)
		}
	}
}

func TestRuntimeCommandServiceUsesClosedCanonicalCommands(t *testing.T) {
	now := time.Date(2026, time.July, 13, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		command   func(*application.RuntimeCommandService, context.Context, application.RuntimeCommandInput) (domain.EnvironmentRuntimeOperation, error)
		wantType  domain.OperationType
		wantInput string
	}{
		{name: "stop", command: (*application.RuntimeCommandService).StopRuntime, wantType: domain.OperationRuntimeStop, wantInput: `{"reason":"manual"}`},
		{name: "replace", command: (*application.RuntimeCommandService).ReplaceRuntime, wantType: domain.OperationRuntimeReplace, wantInput: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment, runtime := stoppedEnvironmentRuntime(t, now.Add(-time.Hour))
			repository := &runtimeCommandRepositoryFake{environment: environment, runtime: runtime}
			dispatcher := &runtimeCommandDispatcherFake{}
			service := application.NewRuntimeCommandService(repository, dispatcher, &idsFake{values: []string{"operation-1"}}, func() time.Time { return now })

			command, err := test.command(service, t.Context(), application.RuntimeCommandInput{
				OwnerUserID: "user-1", EnvironmentID: "environment-1", IdempotencyKey: "request-1",
			})
			if err != nil {
				t.Fatalf("Runtime command: %v", err)
			}
			operation := command.Operation().Snapshot()
			if operation.Type != test.wantType || string(operation.Input) != test.wantInput {
				t.Fatalf("Operation type/input = %q, %s", operation.Type, operation.Input)
			}
		})
	}
}

type runtimeCommandRepositoryFake struct {
	environment domain.Environment
	runtime     domain.Runtime
	operation   domain.Operation
	calls       int
}

func (repository *runtimeCommandRepositoryFake) ReserveRuntimeOperation(_ context.Context, operation domain.Operation) (domain.EnvironmentRuntimeOperation, error) {
	repository.calls++
	if repository.operation.Snapshot().ID == "" {
		repository.operation = operation
	}
	return domain.NewEnvironmentRuntimeOperation(repository.environment, repository.runtime, repository.operation)
}

type runtimeCommandDispatcherFake struct{ operationIDs []string }

func (dispatcher *runtimeCommandDispatcherFake) DispatchRuntimeOperation(_ context.Context, operationID string) error {
	dispatcher.operationIDs = append(dispatcher.operationIDs, operationID)
	return nil
}

func stoppedEnvironmentRuntime(t *testing.T, createdAt time.Time) (domain.Environment, domain.Runtime) {
	t.Helper()
	runtimeID := "runtime-1"
	environment, err := domain.RestoreEnvironment(domain.EnvironmentSnapshot{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev",
		Lifecycle: domain.EnvironmentActive, Health: domain.EnvironmentHealthHealthy,
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-1", CurrentRuntimeID: &runtimeID, AutoStopPolicyID: "policy-1",
		CreatedAt: createdAt, UpdatedAt: createdAt.Add(5 * time.Minute), Version: 3,
	})
	if err != nil {
		t.Fatalf("RestoreEnvironment(): %v", err)
	}
	providerID := "i-runtime-1"
	startedAt, stoppedAt := createdAt.Add(time.Minute), createdAt.Add(4*time.Minute)
	runtime, err := domain.RestoreRuntime(domain.RuntimeSnapshot{
		ID: runtimeID, EnvironmentID: "environment-1", Sequence: 1, Status: domain.RuntimeStopped,
		RuntimePreset: "standard", Region: "us-east-1", AvailabilityZone: "us-east-1a", ImageVersion: "image-1",
		ProviderInstanceRef: &providerID, StartedAt: &startedAt, StoppedAt: &stoppedAt,
		CreatedAt: createdAt, UpdatedAt: stoppedAt, Version: 5,
	})
	if err != nil {
		t.Fatalf("RestoreRuntime(): %v", err)
	}
	return environment, runtime
}
