package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestEnvironmentReservesExactStateComponentsOnOneDataVolume(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 17, 0, 0, 0, time.UTC)
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-1", AutoStopPolicyID: "policy-1", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveEnvironment(): %v", err)
	}

	state, err := domain.ReserveEnvironmentState(environment, environmentCreateOperation(t, createdAt), domain.EnvironmentStateReservation{
		WorkspaceID: "state-workspace", HomeID: "state-home", ServicesID: "state-services", CacheID: "state-cache",
		BackendResourceID: "resource-volume", Provider: "aws",
		ProviderID: "vol-123", Metadata: []byte(`{"encrypted":true}`), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveEnvironmentState(): %v", err)
	}

	components := state.Components()
	want := []struct {
		kind       domain.StateComponentKind
		durability domain.DurabilityClass
		mountPath  string
	}{
		{domain.StateWorkspace, domain.DurabilityDurable, "/workspace"},
		{domain.StateHome, domain.DurabilityDurable, "/home/dev"},
		{domain.StateServices, domain.DurabilityDurable, "/var/lib/docker"},
		{domain.StateCache, domain.DurabilityDisposable, "/var/cache/devm"},
	}
	if len(components) != len(want) {
		t.Fatalf("State Components = %#v", components)
	}
	for index, expected := range want {
		component := components[index]
		if component.EnvironmentID != "environment-1" || component.Kind != expected.kind || component.Durability != expected.durability || component.MountPath != expected.mountPath || component.BackendResourceID != "resource-volume" {
			t.Fatalf("State Component %d = %#v", index, component)
		}
	}
	backend := state.Backend()
	if backend.ID != "resource-volume" || backend.EnvironmentID != "environment-1" || backend.Region != "us-east-1" || backend.ProviderID != "vol-123" {
		t.Fatalf("State backend = %#v", backend)
	}
	if state.DataVolumeProviderID() != "vol-123" {
		t.Fatalf("DataVolumeProviderID() = %q", state.DataVolumeProviderID())
	}
}

func TestRestoreEnvironmentStatePreservesOwnedInventory(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 17, 0, 0, 0, time.UTC)
	state := reservedEnvironmentState(t, createdAt)
	components, backend := state.Components(), state.Backend()
	observedDigest := "sha256:" + strings.Repeat("a", 64)
	components[0].ObservedDigest = &observedDigest
	components[0], components[3] = components[3], components[0]

	restored, err := domain.RestoreEnvironmentState(state.Environment(), environmentCreateOperation(t, createdAt), components, backend)
	if err != nil {
		t.Fatalf("RestoreEnvironmentState(): %v", err)
	}
	components[0].ID = "mutated"
	backend.Metadata[0] = 'x'
	observedDigest = "sha256:" + strings.Repeat("b", 64)
	backend.OperationID = "operation-mutated"
	if got := restored.Components()[0].ID; got != "state-workspace" {
		t.Fatalf("restored State Component changed to %q", got)
	}
	if got := string(restored.Backend().Metadata); got != `{"encrypted":true}` {
		t.Fatalf("restored metadata changed to %q", got)
	}
	returned := restored.Components()
	*returned[0].ObservedDigest = "sha256:" + strings.Repeat("c", 64)
	if got := *restored.Components()[0].ObservedDigest; got != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("restored observed digest changed to %q", got)
	}
	if got := restored.Backend().OperationID; got != "operation-1" {
		t.Fatalf("restored Operation ID changed to %q", got)
	}
}

func TestReserveEnvironmentStateRejectsInvalidOwnershipAndInput(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 17, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*domain.Environment, *domain.Operation, *domain.EnvironmentStateReservation)
	}{
		{name: "active Environment", mutate: func(environment *domain.Environment, _ *domain.Operation, _ *domain.EnvironmentStateReservation) {
			active, err := environment.Activate(createdAt)
			if err != nil {
				t.Fatalf("Activate(): %v", err)
			}
			*environment = active
		}},
		{name: "foreign Environment Operation", mutate: func(_ *domain.Environment, operation *domain.Operation, _ *domain.EnvironmentStateReservation) {
			*operation = operationForState(t, createdAt, "environment-2", "user-1", domain.OperationEnvironmentCreate)
		}},
		{name: "foreign owner Operation", mutate: func(_ *domain.Environment, operation *domain.Operation, _ *domain.EnvironmentStateReservation) {
			*operation = operationForState(t, createdAt, "environment-1", "user-2", domain.OperationEnvironmentCreate)
		}},
		{name: "wrong Operation type", mutate: func(_ *domain.Environment, operation *domain.Operation, _ *domain.EnvironmentStateReservation) {
			*operation = operationForState(t, createdAt, "environment-1", "user-1", domain.OperationRuntimeStart)
		}},
		{name: "terminal Operation", mutate: func(_ *domain.Environment, operation *domain.Operation, _ *domain.EnvironmentStateReservation) {
			snapshot := operation.Snapshot()
			invocationID := "invocation-1"
			completedAt := createdAt.Add(time.Minute)
			snapshot.Status, snapshot.RestateInvocationID, snapshot.CompletedAt = domain.OperationSucceeded, &invocationID, &completedAt
			terminal, err := domain.RestoreOperation(snapshot)
			if err != nil {
				t.Fatalf("RestoreOperation(): %v", err)
			}
			*operation = terminal
		}},
		{name: "duplicate identity", mutate: func(_ *domain.Environment, _ *domain.Operation, reservation *domain.EnvironmentStateReservation) {
			reservation.HomeID = reservation.WorkspaceID
		}},
		{name: "padded provider", mutate: func(_ *domain.Environment, _ *domain.Operation, reservation *domain.EnvironmentStateReservation) {
			reservation.Provider = "aws\t"
		}},
		{name: "invalid metadata", mutate: func(_ *domain.Environment, _ *domain.Operation, reservation *domain.EnvironmentStateReservation) {
			reservation.Metadata = []byte(`[]`)
		}},
		{name: "stale creation", mutate: func(_ *domain.Environment, _ *domain.Operation, reservation *domain.EnvironmentStateReservation) {
			reservation.CreatedAt = createdAt.Add(-time.Second)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := environmentForState(t, createdAt)
			operation := environmentCreateOperation(t, createdAt)
			reservation := validEnvironmentStateReservation(createdAt)
			test.mutate(&environment, &operation, &reservation)
			if _, err := domain.ReserveEnvironmentState(environment, operation, reservation); err == nil {
				t.Fatal("ReserveEnvironmentState() error = nil")
			}
		})
	}
}

func TestRestoreEnvironmentStateRejectsInventoryDrift(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 17, 0, 0, 0, time.UTC)
	state := reservedEnvironmentState(t, createdAt)
	tests := []struct {
		name   string
		mutate func(*[]domain.StateComponentSnapshot, *domain.DataVolumeResourceSnapshot)
	}{
		{name: "missing component", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			*components = (*components)[:3]
		}},
		{name: "extra component", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			extra := (*components)[0]
			extra.ID, extra.Kind = "state-extra", "extra"
			*components = append(*components, extra)
		}},
		{name: "duplicate kind", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			duplicate := (*components)[0]
			duplicate.ID = (*components)[1].ID
			(*components)[1] = duplicate
		}},
		{name: "duplicate identity", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			(*components)[1].ID = (*components)[0].ID
		}},
		{name: "wrong mount", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			(*components)[0].MountPath = "/home/dev"
		}},
		{name: "wrong durability", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			(*components)[3].Durability = domain.DurabilityDurable
		}},
		{name: "foreign component", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			(*components)[0].EnvironmentID = "environment-2"
		}},
		{name: "wrong backend", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			(*components)[0].BackendResourceID = "resource-other"
		}},
		{name: "invalid observed digest", mutate: func(components *[]domain.StateComponentSnapshot, _ *domain.DataVolumeResourceSnapshot) {
			value := "sha256:nope"
			(*components)[0].ObservedDigest = &value
		}},
		{name: "foreign backend region", mutate: func(_ *[]domain.StateComponentSnapshot, backend *domain.DataVolumeResourceSnapshot) {
			backend.Region = "eu-west-1"
		}},
		{name: "foreign backend Operation", mutate: func(_ *[]domain.StateComponentSnapshot, backend *domain.DataVolumeResourceSnapshot) {
			backend.OperationID = "operation-2"
		}},
		{name: "non-object metadata", mutate: func(_ *[]domain.StateComponentSnapshot, backend *domain.DataVolumeResourceSnapshot) {
			backend.Metadata = []byte(`[]`)
		}},
		{name: "deleted backend", mutate: func(_ *[]domain.StateComponentSnapshot, backend *domain.DataVolumeResourceSnapshot) {
			value := createdAt.Add(time.Minute)
			backend.DeletedAt = &value
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			components, backend := state.Components(), state.Backend()
			test.mutate(&components, &backend)
			if _, err := domain.RestoreEnvironmentState(state.Environment(), environmentCreateOperation(t, createdAt), components, backend); err == nil {
				t.Fatal("RestoreEnvironmentState() error = nil")
			}
		})
	}
}

func reservedEnvironmentState(t *testing.T, createdAt time.Time) domain.EnvironmentState {
	t.Helper()
	environment := environmentForState(t, createdAt)
	state, err := domain.ReserveEnvironmentState(environment, environmentCreateOperation(t, createdAt), validEnvironmentStateReservation(createdAt))
	if err != nil {
		t.Fatalf("ReserveEnvironmentState(): %v", err)
	}
	return state
}

func environmentForState(t *testing.T, createdAt time.Time) domain.Environment {
	t.Helper()
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-1", AutoStopPolicyID: "policy-1", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveEnvironment(): %v", err)
	}
	return environment
}

func validEnvironmentStateReservation(createdAt time.Time) domain.EnvironmentStateReservation {
	return domain.EnvironmentStateReservation{
		WorkspaceID: "state-workspace", HomeID: "state-home", ServicesID: "state-services", CacheID: "state-cache",
		BackendResourceID: "resource-volume", Provider: "aws",
		ProviderID: "vol-123", Metadata: []byte(`{"encrypted":true}`), CreatedAt: createdAt,
	}
}

func environmentCreateOperation(t *testing.T, createdAt time.Time) domain.Operation {
	t.Helper()
	return operationForState(t, createdAt, "environment-1", "user-1", domain.OperationEnvironmentCreate)
}

func operationForState(t *testing.T, createdAt time.Time, environmentID, userID string, operationType domain.OperationType) domain.Operation {
	t.Helper()
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: "operation-1", EnvironmentID: environmentID, Type: operationType,
		RequestedByUserID: userID, IdempotencyKey: "request-1", Input: []byte(`{}`), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("QueueOperation(): %v", err)
	}
	return operation
}
