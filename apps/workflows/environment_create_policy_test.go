package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
)

func TestClassifyDurableErrorTerminatesOnlyClassifiedPermanentFailures(t *testing.T) {
	permanent := classifiedTestError{err: errors.New("conflict")}
	if err := classifyDurableError(permanent); !restate.IsTerminalError(err) {
		t.Fatalf("permanent error classification = %T, want TerminalError", err)
	}
	transient := classifiedTestError{err: errors.New("unavailable"), transient: true}
	if err := classifyDurableError(transient); !errors.Is(err, transient.err) || restate.IsTerminalError(err) {
		t.Fatalf("transient error classification = %T %v", err, err)
	}
	unknown := errors.New("unknown infrastructure failure")
	if err := classifyDurableError(unknown); err != unknown {
		t.Fatalf("unknown error classification = %T %v", err, err)
	}
}

func TestEnvironmentCreationActionsReturnsPersistedProviderIdentity(t *testing.T) {
	createdAt := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	repository := &creationRepositoryFake{state: validEnvironmentState(t, createdAt, "persisted-volume-1")}
	actions := &environmentCreationActions{repository: repository}

	providerID, err := actions.InventoryEnvironmentState(t.Context(), "operation-1", domain.EnvironmentStateReservation{})
	if err != nil {
		t.Fatalf("inventory Environment State: %v", err)
	}
	if providerID != "persisted-volume-1" {
		t.Fatalf("authoritative provider ID = %q", providerID)
	}
}

func TestNewEnvironmentCreationActionsRequiresPinnedProfileResolver(t *testing.T) {
	actions, err := NewEnvironmentCreationActions(&creationRepositoryFake{}, nil)
	if err == nil || actions != nil {
		t.Fatalf("NewEnvironmentCreationActions() = actions:%T error:%v, want loud missing-resolver failure", actions, err)
	}
}

func TestValidateDataVolumeRejectsOwnershipPlacementAndNonCanonicalIdentity(t *testing.T) {
	input := domain.EnvironmentCreateDispatch{
		EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a",
	}
	valid := provider.DataVolume{
		Provider: "aws", ProviderID: "volume-1", EnvironmentID: input.EnvironmentID,
		Region: input.Region, AvailabilityZone: input.AvailabilityZone,
	}
	if err := validateDataVolume(input, valid); err != nil {
		t.Fatalf("valid Data Volume: %v", err)
	}
	for name, mutate := range map[string]func(*provider.DataVolume){
		"Environment":        func(volume *provider.DataVolume) { volume.EnvironmentID = "environment-2" },
		"region":             func(volume *provider.DataVolume) { volume.Region = "us-west-2" },
		"Availability Zone":  func(volume *provider.DataVolume) { volume.AvailabilityZone = "us-east-1b" },
		"blank provider":     func(volume *provider.DataVolume) { volume.Provider = "" },
		"padded provider":    func(volume *provider.DataVolume) { volume.Provider = " aws" },
		"blank provider ID":  func(volume *provider.DataVolume) { volume.ProviderID = "" },
		"padded provider ID": func(volume *provider.DataVolume) { volume.ProviderID = "volume-1 " },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := validateDataVolume(input, candidate); !restate.IsTerminalError(err) {
				t.Fatalf("invalid Data Volume error = %T %v", err, err)
			}
		})
	}
}

type classifiedTestError struct {
	err       error
	transient bool
}

func (err classifiedTestError) Error() string   { return err.err.Error() }
func (err classifiedTestError) Unwrap() error   { return err.err }
func (err classifiedTestError) Transient() bool { return err.transient }

type creationRepositoryFake struct {
	state domain.EnvironmentState
}

func (*creationRepositoryFake) RecordEnvironmentCreateInvocation(context.Context, string, string, time.Time) (domain.EnvironmentCreation, error) {
	return domain.EnvironmentCreation{}, nil
}

func (fake *creationRepositoryFake) InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (domain.EnvironmentState, error) {
	return fake.state, nil
}

func (*creationRepositoryFake) ReserveInitialRuntime(context.Context, string, domain.RuntimeReservation) (domain.Runtime, error) {
	return domain.Runtime{}, nil
}

func (*creationRepositoryFake) PersistEnvironmentCreateRuntimeTransition(context.Context, string, int64, domain.RuntimeSnapshot) error {
	return nil
}

func (*creationRepositoryFake) FinishEnvironmentCreateOperation(context.Context, string, domain.OperationStatus, string, string, time.Time) error {
	return nil
}

func (*creationRepositoryFake) CompleteEnvironmentCreation(context.Context, string, time.Time) (domain.EnvironmentCreation, error) {
	return domain.EnvironmentCreation{}, nil
}

func validEnvironmentState(t *testing.T, createdAt time.Time, providerID string) domain.EnvironmentState {
	t.Helper()
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		PinnedProfileVersionID: "profile-1", AutoStopPolicyID: "policy-1", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: "operation-1", EnvironmentID: "environment-1", Type: domain.OperationEnvironmentCreate,
		RequestedByUserID: "user-1", IdempotencyKey: "request-1", Input: []byte(`{}`), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("queue Operation: %v", err)
	}
	state, err := domain.ReserveEnvironmentState(environment, operation, domain.EnvironmentStateReservation{
		BackendResourceID: "resource-1", WorkspaceID: "workspace-1", HomeID: "home-1",
		ServicesID: "services-1", CacheID: "cache-1", Provider: "aws", ProviderID: providerID,
		Metadata: []byte(`{"availabilityZone":"us-east-1a"}`), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("reserve Environment State: %v", err)
	}
	return state
}
