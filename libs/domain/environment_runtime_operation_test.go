package domain_test

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestEnvironmentAcceptsRuntimeOperationForItsCurrentRuntime(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 14, 0, 0, 0, time.UTC)
	reserved := reservedRuntime(t, createdAt)
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-1", AutoStopPolicyID: "policy-1", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveEnvironment(): %v", err)
	}
	environment, err = environment.Activate(createdAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("Activate(): %v", err)
	}
	environment, err = environment.AttachRuntime(reserved, createdAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("AttachRuntime(): %v", err)
	}
	runtime := stoppedRuntime(t, createdAt)
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: "operation-1", EnvironmentID: "environment-1", Type: domain.OperationRuntimeStart,
		RequestedByUserID: "user-1", IdempotencyKey: "request-1", Input: []byte(`{}`),
		CreatedAt: createdAt.Add(6 * time.Minute),
	})
	if err != nil {
		t.Fatalf("QueueOperation(): %v", err)
	}

	command, err := domain.NewEnvironmentRuntimeOperation(environment, runtime, operation)
	if err != nil {
		t.Fatalf("NewEnvironmentRuntimeOperation(): %v", err)
	}

	if got := command.Environment().Snapshot().ID; got != "environment-1" {
		t.Fatalf("Environment ID = %q", got)
	}
	if got := command.Runtime().Snapshot().ID; got != "runtime-1" {
		t.Fatalf("Runtime ID = %q", got)
	}
	if got := command.Operation().Snapshot().Type; got != domain.OperationRuntimeStart {
		t.Fatalf("Operation type = %q", got)
	}
}

func TestEnvironmentRejectsRuntimeOperationOutsideCurrentOwnershipAndLifecycle(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 14, 0, 0, 0, time.UTC)
	environment, runtime, operation := runtimeOperationFixture(t, createdAt, domain.OperationRuntimeStart)

	tests := []struct {
		name        string
		environment domain.Environment
		runtime     domain.Runtime
		operation   domain.Operation
	}{
		{name: "non-current Runtime", environment: environment, runtime: reserveReplacementRuntime(t, runtime.Snapshot(), createdAt.Add(7*time.Minute)), operation: operation},
		{name: "foreign Runtime ownership", environment: environment, runtime: runtimeWithEnvironment(t, runtime, "environment-2"), operation: operation},
		{name: "foreign Operation Environment", environment: environment, runtime: runtime, operation: runtimeOperation(t, createdAt.Add(6*time.Minute), "environment-2", "user-1", domain.OperationRuntimeStart)},
		{name: "foreign requesting User", environment: environment, runtime: runtime, operation: runtimeOperation(t, createdAt.Add(6*time.Minute), "environment-1", "user-2", domain.OperationRuntimeStart)},
		{name: "non-runtime Operation", environment: environment, runtime: runtime, operation: runtimeOperation(t, createdAt.Add(6*time.Minute), "environment-1", "user-1", domain.OperationEnvironmentCreate)},
		{name: "non-active Environment", environment: creatingEnvironmentWithRuntime(t, createdAt), runtime: runtime, operation: operation},
		{name: "Runtime state cannot start", environment: environment, runtime: reservedRuntime(t, createdAt), operation: operation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := domain.NewEnvironmentRuntimeOperation(test.environment, test.runtime, test.operation); err == nil {
				t.Fatal("NewEnvironmentRuntimeOperation() error = nil")
			}
		})
	}
}

func TestEnvironmentRuntimeOperationEligibilityMatchesLifecycle(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 14, 0, 0, 0, time.UTC)
	environment, _, _ := runtimeOperationFixture(t, createdAt, domain.OperationRuntimeStart)
	ready, _ := readyRuntime(t, createdAt)
	failed, err := ready.MarkError(domain.RuntimeStateObservation{
		ProviderInstanceRef: "i-runtime-1", ExpectedVersion: ready.Snapshot().Version,
		ObservedAt: createdAt.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("MarkError(): %v", err)
	}
	stopping, err := ready.BeginStop(createdAt.Add(4 * time.Minute))
	if err != nil {
		t.Fatalf("BeginStop(): %v", err)
	}
	replacing, err := ready.BeginReplacement(createdAt.Add(4 * time.Minute))
	if err != nil {
		t.Fatalf("BeginReplacement(): %v", err)
	}

	tests := []struct {
		name          string
		operationType domain.OperationType
		runtime       domain.Runtime
		running       bool
		wantAllowed   bool
	}{
		{name: "start stopped", operationType: domain.OperationRuntimeStart, runtime: stoppedRuntime(t, createdAt), wantAllowed: true},
		{name: "start ready replay", operationType: domain.OperationRuntimeStart, runtime: ready, wantAllowed: true},
		{name: "start in flight replay", operationType: domain.OperationRuntimeStart, runtime: startingRuntime(t, createdAt), running: true, wantAllowed: true},
		{name: "start in flight new command", operationType: domain.OperationRuntimeStart, runtime: startingRuntime(t, createdAt)},
		{name: "start absent", operationType: domain.OperationRuntimeStart, runtime: reservedRuntime(t, createdAt)},
		{name: "stop ready", operationType: domain.OperationRuntimeStop, runtime: ready, wantAllowed: true},
		{name: "stop stopped replay", operationType: domain.OperationRuntimeStop, runtime: stoppedRuntime(t, createdAt), wantAllowed: true},
		{name: "stop in flight replay", operationType: domain.OperationRuntimeStop, runtime: stopping, running: true, wantAllowed: true},
		{name: "stop starting", operationType: domain.OperationRuntimeStop, runtime: startingRuntime(t, createdAt)},
		{name: "replace ready", operationType: domain.OperationRuntimeReplace, runtime: ready, wantAllowed: true},
		{name: "replace stopped", operationType: domain.OperationRuntimeReplace, runtime: stoppedRuntime(t, createdAt), wantAllowed: true},
		{name: "replace error", operationType: domain.OperationRuntimeReplace, runtime: failed, wantAllowed: true},
		{name: "replace in flight replay", operationType: domain.OperationRuntimeReplace, runtime: replacing, running: true, wantAllowed: true},
		{name: "replace starting", operationType: domain.OperationRuntimeReplace, runtime: startingRuntime(t, createdAt)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := runtimeOperation(t, createdAt.Add(10*time.Minute), "environment-1", "user-1", test.operationType)
			if test.running {
				operation, err = operation.Start(createdAt.Add(11 * time.Minute))
				if err != nil {
					t.Fatalf("Start Operation: %v", err)
				}
			}
			_, err := domain.NewEnvironmentRuntimeOperation(environment, test.runtime, operation)
			if (err == nil) != test.wantAllowed {
				t.Fatalf("NewEnvironmentRuntimeOperation() error = %v, allowed = %t", err, test.wantAllowed)
			}
		})
	}
}

func TestEnvironmentAcceptsSucceededRuntimeOperationAsHistoricalReplay(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 14, 0, 0, 0, time.UTC)
	environment, _, _ := runtimeOperationFixture(t, createdAt, domain.OperationRuntimeStart)
	operation := runtimeOperation(t, createdAt.Add(10*time.Minute), "environment-1", "user-1", domain.OperationRuntimeStart)
	var err error
	operation, err = operation.Start(createdAt.Add(11 * time.Minute))
	if err != nil {
		t.Fatalf("Start Operation: %v", err)
	}
	operation, err = operation.RecordRestateInvocation("invocation-1")
	if err != nil {
		t.Fatalf("RecordRestateInvocation(): %v", err)
	}
	operation, err = operation.Succeed(createdAt.Add(12 * time.Minute))
	if err != nil {
		t.Fatalf("Succeed(): %v", err)
	}

	if _, err := domain.NewEnvironmentRuntimeOperation(environment, startingRuntime(t, createdAt), operation); err != nil {
		t.Fatalf("historical replay: %v", err)
	}
	environmentSnapshot := environment.Snapshot()
	replacementID := "runtime-2"
	environmentSnapshot.CurrentRuntimeID = &replacementID
	environmentSnapshot.Version++
	environmentSnapshot.UpdatedAt = createdAt.Add(13 * time.Minute)
	replacedEnvironment, err := domain.RestoreEnvironment(environmentSnapshot)
	if err != nil {
		t.Fatalf("RestoreEnvironment(): %v", err)
	}
	for _, status := range []domain.OperationStatus{
		domain.OperationSucceeded, domain.OperationFailed, domain.OperationCancelled, domain.OperationBlocked,
	} {
		snapshot := operation.Snapshot()
		snapshot.Status = status
		terminal, err := domain.RestoreOperation(snapshot)
		if err != nil {
			t.Fatalf("RestoreOperation(%q): %v", status, err)
		}
		if _, err := domain.NewEnvironmentRuntimeOperation(replacedEnvironment, stoppedRuntime(t, createdAt), terminal); err != nil {
			t.Fatalf("historical %q target replay after replacement: %v", status, err)
		}
	}
}

func runtimeOperationFixture(t *testing.T, createdAt time.Time, operationType domain.OperationType) (domain.Environment, domain.Runtime, domain.Operation) {
	t.Helper()
	reserved := reservedRuntime(t, createdAt)
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-1", AutoStopPolicyID: "policy-1", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveEnvironment(): %v", err)
	}
	environment, err = environment.Activate(createdAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("Activate(): %v", err)
	}
	environment, err = environment.AttachRuntime(reserved, createdAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("AttachRuntime(): %v", err)
	}
	runtime := stoppedRuntime(t, createdAt)
	return environment, runtime, runtimeOperation(t, createdAt.Add(6*time.Minute), "environment-1", "user-1", operationType)
}

func runtimeOperation(t *testing.T, at time.Time, environmentID, userID string, operationType domain.OperationType) domain.Operation {
	t.Helper()
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: "operation-1", EnvironmentID: environmentID, Type: operationType,
		RequestedByUserID: userID, IdempotencyKey: "request-1", Input: []byte(`{}`), CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("QueueOperation(): %v", err)
	}
	return operation
}

func stoppedRuntime(t *testing.T, createdAt time.Time) domain.Runtime {
	t.Helper()
	ready, _ := readyRuntime(t, createdAt)
	stopping, err := ready.BeginStop(createdAt.Add(4 * time.Minute))
	if err != nil {
		t.Fatalf("BeginStop(): %v", err)
	}
	stopped, err := stopping.MarkStopped(domain.RuntimeStateObservation{
		ProviderInstanceRef: "i-runtime-1", ExpectedVersion: stopping.Snapshot().Version,
		ObservedAt: createdAt.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("MarkStopped(): %v", err)
	}
	return stopped
}

func runtimeWithEnvironment(t *testing.T, runtime domain.Runtime, environmentID string) domain.Runtime {
	t.Helper()
	snapshot := runtime.Snapshot()
	snapshot.EnvironmentID = environmentID
	foreign, err := domain.RestoreRuntime(snapshot)
	if err != nil {
		t.Fatalf("RestoreRuntime(): %v", err)
	}
	return foreign
}

func creatingEnvironmentWithRuntime(t *testing.T, createdAt time.Time) domain.Environment {
	t.Helper()
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-1", AutoStopPolicyID: "policy-1", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveEnvironment(): %v", err)
	}
	environment, err = environment.AttachRuntime(reservedRuntime(t, createdAt), createdAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("AttachRuntime(): %v", err)
	}
	return environment
}
