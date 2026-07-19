//go:build !race

package workflows_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/restatedev/sdk-go/ingress"
)

func TestEnvironmentCreateWorkflowResumesAfterHandlerTerminationAtEveryDurableBoundary(t *testing.T) {
	for _, boundary := range []string{"record", "provider", "seed", "inventory", "runtime", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			gate := newTerminationGate(boundary)
			actions := newResumableCreationActions(gate)
			dataVolumes := &resumableDataVolumeProvider{gate: gate, provider: testfixtures.NewProvider()}
			ids := &resumableIDs{gate: gate, values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}}
			environment := testfixtures.StartRestate(t, environmentCreateDefinition(dataVolumes, actions, ids, time.Now, "image-v1"))
			input := domain.EnvironmentCreateDispatch{
				OperationID: "operation-" + boundary, EnvironmentID: "environment-" + boundary,
				Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
			}
			if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
				t.Fatalf("submit Environment create workflow: %v", err)
			}
			select {
			case <-gate.reached:
			case <-time.After(5 * time.Second):
				t.Fatalf("workflow did not reach %s boundary", boundary)
			}
			environment.TerminateEndpoint()
			close(gate.release)
			select {
			case <-gate.passed:
			case <-time.After(time.Second):
				t.Fatalf("%s action did not return into terminated endpoint", boundary)
			}
			environment.ResumeEndpoint()

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			output, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
				environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
			).Attach(ctx)
			if err != nil {
				t.Fatalf("resume Environment create workflow: %v", err)
			}
			if output.DataVolumeProviderID != "fake-volume-"+input.EnvironmentID || output.RuntimeID != "runtime-1" {
				t.Fatalf("resumed workflow output = %#v", output)
			}
			if record, inventory, runtime, complete := actions.appliedCounts(); record != 1 || inventory != 1 || runtime != 1 || complete != 1 {
				t.Fatalf("persisted mutations after resume = record:%d inventory:%d runtime:%d complete:%d", record, inventory, runtime, complete)
			}
			if dataVolumes.provider.DataVolumeCreateCount() != 1 {
				t.Fatalf("provider mutations after resume = %d", dataVolumes.provider.DataVolumeCreateCount())
			}
			if attempts := boundaryAttempts(boundary, actions, dataVolumes, ids); attempts < 2 {
				t.Fatalf("%s action attempts after endpoint termination = %d, want at least 2", boundary, attempts)
			}
			reservation := actions.persistedReservation()
			if reservation.BackendResourceID != "resource-1" || reservation.WorkspaceID != "workspace-1" ||
				reservation.HomeID != "home-1" || reservation.ServicesID != "services-1" || reservation.CacheID != "cache-1" {
				t.Fatalf("persisted reservation after resume = %#v", reservation)
			}
			for attempt, candidate := range actions.inventoryReservations() {
				if candidate.BackendResourceID != reservation.BackendResourceID || candidate.WorkspaceID != reservation.WorkspaceID ||
					candidate.HomeID != reservation.HomeID || candidate.ServicesID != reservation.ServicesID || candidate.CacheID != reservation.CacheID ||
					candidate.Provider != reservation.Provider || candidate.ProviderID != reservation.ProviderID || !candidate.CreatedAt.Equal(reservation.CreatedAt) {
					t.Fatalf("inventory reservation attempt %d changed after resume: %#v", attempt+1, candidate)
				}
			}
			runtime := actions.persistedRuntimeReservation()
			for attempt, candidate := range actions.runtimeReservations() {
				if candidate.ID != runtime.ID || candidate.EnvironmentID != runtime.EnvironmentID || candidate.Sequence != runtime.Sequence ||
					candidate.RuntimePreset != runtime.RuntimePreset || candidate.Region != runtime.Region ||
					candidate.AvailabilityZone != runtime.AvailabilityZone || candidate.ImageVersion != runtime.ImageVersion ||
					!candidate.CreatedAt.Equal(runtime.CreatedAt) {
					t.Fatalf("Runtime reservation attempt %d changed after resume: %#v", attempt+1, candidate)
				}
			}
		})
	}
}

func boundaryAttempts(boundary string, actions *resumableCreationActions, dataVolumes *resumableDataVolumeProvider, ids *resumableIDs) int {
	switch boundary {
	case "record":
		return actions.attemptCount(boundary)
	case "provider":
		return dataVolumes.attemptCount()
	case "seed":
		return ids.attemptCount()
	case "inventory", "runtime", "complete":
		return actions.attemptCount(boundary)
	default:
		return 0
	}
}

type terminationGate struct {
	mu        sync.Mutex
	target    string
	triggered bool
	reached   chan struct{}
	release   chan struct{}
	passed    chan struct{}
}

func newTerminationGate(target string) *terminationGate {
	return &terminationGate{target: target, reached: make(chan struct{}), release: make(chan struct{}), passed: make(chan struct{})}
}

func (gate *terminationGate) hit(boundary string) {
	gate.mu.Lock()
	if gate.target != boundary || gate.triggered {
		gate.mu.Unlock()
		return
	}
	gate.triggered = true
	close(gate.reached)
	gate.mu.Unlock()
	<-gate.release
	close(gate.passed)
}

type resumableCreationActions struct {
	gate *terminationGate
	mu   sync.Mutex

	recorded, inventoried, runtimeReserved, completed                    bool
	recordApplies, inventoryApplies, runtimeApplies, completeApplies     int
	recordAttempts, inventoryAttempts, runtimeAttempts, completeAttempts int
	reservation                                                          domain.EnvironmentStateReservation
	reservations                                                         []domain.EnvironmentStateReservation
	runtimeReservation                                                   domain.RuntimeReservation
	runtimeReservationAttempts                                           []domain.RuntimeReservation
}

func newResumableCreationActions(gate *terminationGate) *resumableCreationActions {
	return &resumableCreationActions{gate: gate}
}

func (actions *resumableCreationActions) RecordEnvironmentCreateInvocation(_ context.Context, operationID string, _ string, _ time.Time) (workflows.EnvironmentCreateInvocation, error) {
	actions.mu.Lock()
	actions.recordAttempts++
	if !actions.recorded {
		actions.recorded = true
		actions.recordApplies++
	}
	actions.mu.Unlock()
	actions.gate.hit("record")
	return workflows.EnvironmentCreateInvocation{
		OwnerUserID: "user-1", EnvironmentID: strings.Replace(operationID, "operation", "environment", 1), ProjectSeedID: "project-seed-1",
	}, nil
}

func (actions *resumableCreationActions) InventoryEnvironmentState(_ context.Context, _ string, reservation domain.EnvironmentStateReservation) (string, error) {
	actions.mu.Lock()
	actions.inventoryAttempts++
	actions.reservations = append(actions.reservations, reservation)
	if !actions.inventoried {
		actions.inventoried = true
		actions.inventoryApplies++
		actions.reservation = reservation
	}
	actions.mu.Unlock()
	actions.gate.hit("inventory")
	return reservation.ProviderID, nil
}

func (actions *resumableCreationActions) ReserveInitialRuntime(_ context.Context, _ string, reservation domain.RuntimeReservation) (string, error) {
	actions.mu.Lock()
	actions.runtimeAttempts++
	actions.runtimeReservationAttempts = append(actions.runtimeReservationAttempts, reservation)
	if !actions.runtimeReserved {
		actions.runtimeReserved = true
		actions.runtimeApplies++
		actions.runtimeReservation = reservation
	}
	persistedID := actions.runtimeReservation.ID
	actions.mu.Unlock()
	actions.gate.hit("runtime")
	return persistedID, nil
}

func (actions *resumableCreationActions) CompleteEnvironmentCreation(context.Context, string, time.Time) error {
	actions.mu.Lock()
	actions.completeAttempts++
	if !actions.completed {
		actions.completed = true
		actions.completeApplies++
	}
	actions.mu.Unlock()
	actions.gate.hit("complete")
	return nil
}

func (actions *resumableCreationActions) appliedCounts() (int, int, int, int) {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	return actions.recordApplies, actions.inventoryApplies, actions.runtimeApplies, actions.completeApplies
}

func (actions *resumableCreationActions) persistedReservation() domain.EnvironmentStateReservation {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	return actions.reservation
}

func (actions *resumableCreationActions) attemptCount(boundary string) int {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	switch boundary {
	case "record":
		return actions.recordAttempts
	case "inventory":
		return actions.inventoryAttempts
	case "runtime":
		return actions.runtimeAttempts
	case "complete":
		return actions.completeAttempts
	default:
		return 0
	}
}

func (actions *resumableCreationActions) persistedRuntimeReservation() domain.RuntimeReservation {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	return actions.runtimeReservation
}

func (actions *resumableCreationActions) runtimeReservations() []domain.RuntimeReservation {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	return append([]domain.RuntimeReservation(nil), actions.runtimeReservationAttempts...)
}

func (actions *resumableCreationActions) inventoryReservations() []domain.EnvironmentStateReservation {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	return append([]domain.EnvironmentStateReservation(nil), actions.reservations...)
}

type resumableDataVolumeProvider struct {
	gate     *terminationGate
	provider *testfixtures.Provider
	mu       sync.Mutex
	attempts int
}

func (dataVolumes *resumableDataVolumeProvider) EnsureDataVolume(ctx context.Context, request provider.EnsureDataVolumeRequest) (provider.DataVolume, error) {
	dataVolumes.mu.Lock()
	dataVolumes.attempts++
	dataVolumes.mu.Unlock()
	volume, err := dataVolumes.provider.EnsureDataVolume(ctx, request)
	if err == nil {
		dataVolumes.gate.hit("provider")
	}
	return volume, err
}

func (dataVolumes *resumableDataVolumeProvider) attemptCount() int {
	dataVolumes.mu.Lock()
	defer dataVolumes.mu.Unlock()
	return dataVolumes.attempts
}

type resumableIDs struct {
	gate   *terminationGate
	mu     sync.Mutex
	values []string
	calls  int
}

func (ids *resumableIDs) NewID() string {
	ids.mu.Lock()
	value := ids.values[ids.calls%len(ids.values)]
	ids.calls++
	completedAttempt := ids.calls%len(ids.values) == 0
	ids.mu.Unlock()
	if completedAttempt {
		ids.gate.hit("seed")
	}
	return value
}

func (ids *resumableIDs) attemptCount() int {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	return ids.calls / len(ids.values)
}
