package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestEnvironmentCreationReservesItsInitialRuntime(t *testing.T) {
	createdAt := validEnvironmentReservation().CreatedAt
	environment, policy, operation := creationParts(t, "env_01", "usr_01", "policy_01")
	creation, err := domain.NewEnvironmentCreation(environment, policy, operation, "seed_01", []string{"key_01"})
	if err != nil {
		t.Fatalf("NewEnvironmentCreation(): %v", err)
	}

	creation, runtime, err := creation.ReserveInitialRuntime(domain.RuntimeReservation{
		ID: "runtime_01", EnvironmentID: "env_01", Sequence: 1,
		RuntimePreset: "standard", Region: "us-east-1", AvailabilityZone: "us-east-1a",
		ImageVersion: "image-v1", CreatedAt: createdAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ReserveInitialRuntime(): %v", err)
	}
	runtimeSnapshot := runtime.Snapshot()
	if runtimeSnapshot.ID != "runtime_01" || runtimeSnapshot.Status != domain.RuntimeAbsent || runtimeSnapshot.Sequence != 1 {
		t.Fatalf("reserved Runtime = %#v", runtimeSnapshot)
	}
	currentRuntimeID := creation.Environment().Snapshot().CurrentRuntimeID
	if currentRuntimeID == nil || *currentRuntimeID != runtimeSnapshot.ID {
		t.Fatalf("Environment current Runtime = %v", currentRuntimeID)
	}
}

func TestEnvironmentCreationRejectsRepeatedInitialRuntimeReservation(t *testing.T) {
	createdAt := validEnvironmentReservation().CreatedAt
	environment, policy, operation := creationParts(t, "env_01", "usr_01", "policy_01")
	creation, err := domain.NewEnvironmentCreation(environment, policy, operation, "seed_01", []string{"key_01"})
	if err != nil {
		t.Fatalf("NewEnvironmentCreation(): %v", err)
	}
	reservation := domain.RuntimeReservation{
		ID: "runtime_01", EnvironmentID: "env_01", Sequence: 1,
		RuntimePreset: "standard", Region: "us-east-1", AvailabilityZone: "us-east-1a",
		ImageVersion: "image-v1", CreatedAt: createdAt.Add(time.Minute),
	}
	creation, _, err = creation.ReserveInitialRuntime(reservation)
	if err != nil {
		t.Fatalf("first ReserveInitialRuntime(): %v", err)
	}
	reservation.ImageVersion = "image-v2"
	if _, _, err := creation.ReserveInitialRuntime(reservation); !errors.Is(err, domain.ErrInitialRuntimeAlreadyReserved) {
		t.Fatalf("repeated ReserveInitialRuntime() error = %v", err)
	}
}

func TestEnvironmentCreationCannotCompleteWithoutInitialRuntime(t *testing.T) {
	createdAt := validEnvironmentReservation().CreatedAt
	environment, policy, operation := creationParts(t, "env_01", "usr_01", "policy_01")
	creation, err := domain.NewEnvironmentCreation(environment, policy, operation, "seed_01", []string{"key_01"})
	if err != nil {
		t.Fatalf("NewEnvironmentCreation(): %v", err)
	}
	creation, err = creation.RecordRestateInvocation("invocation_01")
	if err != nil {
		t.Fatalf("RecordRestateInvocation(): %v", err)
	}

	if _, err := creation.Complete(createdAt.Add(time.Minute)); err == nil {
		t.Fatal("Complete() without initial Runtime error = nil")
	}
}

func TestNewEnvironmentCreationOwnsCoherentReservation(t *testing.T) {
	environment, policy, operation := creationParts(t, "env_01", "usr_01", "policy_01")
	creation, err := domain.NewEnvironmentCreation(environment, policy, operation, "seed_01", []string{"key_01"})
	if err != nil {
		t.Fatalf("NewEnvironmentCreation(): %v", err)
	}
	if creation.Environment().Snapshot().ID != "env_01" || creation.Policy().Snapshot().ID != "policy_01" || creation.Operation().Snapshot().EnvironmentID != "env_01" {
		t.Fatalf("creation = %#v", creation)
	}
	keys := creation.SSHKeyIDs()
	keys[0] = "changed"
	if creation.SSHKeyIDs()[0] != "key_01" {
		t.Fatal("SSH Key IDs escaped aggregate ownership")
	}
}

func TestNewEnvironmentCreationRejectsCrossAggregateMismatch(t *testing.T) {
	environment, policy, operation := creationParts(t, "env_01", "usr_01", "policy_01")
	foreignPolicyEnvironment, err := domain.NewAutoStopPolicy("policy_01", "env_02", domain.AutoStopManual, 0)
	if err != nil {
		t.Fatalf("create foreign policy: %v", err)
	}
	wrongPolicy, err := domain.NewAutoStopPolicy("policy_02", "env_01", domain.AutoStopManual, 0)
	if err != nil {
		t.Fatalf("create wrong policy: %v", err)
	}
	_, _, foreignOperation := creationParts(t, "env_02", "usr_01", "policy_02")
	_, _, foreignUserOperation := creationParts(t, "env_01", "usr_02", "policy_01")

	tests := []struct {
		name        string
		policy      domain.AutoStopPolicy
		operation   domain.Operation
		projectSeed string
		keys        []string
	}{
		{name: "policy target", policy: foreignPolicyEnvironment, operation: operation, projectSeed: "seed_01", keys: []string{"key_01"}},
		{name: "policy identity", policy: wrongPolicy, operation: operation, projectSeed: "seed_01", keys: []string{"key_01"}},
		{name: "Operation target", policy: policy, operation: foreignOperation, projectSeed: "seed_01", keys: []string{"key_01"}},
		{name: "Operation owner", policy: policy, operation: foreignUserOperation, projectSeed: "seed_01", keys: []string{"key_01"}},
		{name: "Project Seed", policy: policy, operation: operation, keys: []string{"key_01"}},
		{name: "SSH Keys", policy: policy, operation: operation, projectSeed: "seed_01"},
		{name: "duplicate SSH Key", policy: policy, operation: operation, projectSeed: "seed_01", keys: []string{"key_01", "key_01"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := domain.NewEnvironmentCreation(environment, test.policy, test.operation, test.projectSeed, test.keys); err == nil {
				t.Fatal("NewEnvironmentCreation() error = nil")
			}
		})
	}
}

func creationParts(t *testing.T, environmentID, ownerID, policyID string) (domain.Environment, domain.AutoStopPolicy, domain.Operation) {
	t.Helper()
	reservation := validEnvironmentReservation()
	reservation.ID, reservation.OwnerUserID, reservation.AutoStopPolicyID = environmentID, ownerID, policyID
	environment, err := domain.ReserveEnvironment(reservation)
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	policy, err := domain.NewAutoStopPolicy(policyID, environmentID, domain.AutoStopManual, 0)
	if err != nil {
		t.Fatalf("create Auto-stop Policy: %v", err)
	}
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: "op_" + environmentID + ownerID, EnvironmentID: environmentID,
		Type: domain.OperationEnvironmentCreate, RequestedByUserID: ownerID,
		IdempotencyKey: "idempotency-key-01", Input: []byte(`{}`), CreatedAt: reservation.CreatedAt,
	})
	if err != nil {
		t.Fatalf("queue Operation: %v", err)
	}
	return environment, policy, operation
}
