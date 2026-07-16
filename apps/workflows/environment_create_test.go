//go:build !race

// Restate SDK v1.0.0's test HTTP/2 server races in its request-body drain path.
// Keep the real-server workflow test in normal tests; race-test sshai adapters separately.
package workflows_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/restatedev/sdk-go/ingress"
)

func TestEnvironmentCreateWorkflowRunsDurableProviderAndCompletionActionsOnce(t *testing.T) {
	provider := testfixtures.NewProvider()
	completion := &completionFake{persistedProviderID: "persisted-volume-1"}
	ids := &workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}}
	completedAt := time.Date(2026, time.July, 13, 12, 1, 0, 0, time.UTC)
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinition(provider, completion, ids, func() time.Time { return completedAt }, "image-v1"))
	client := workflows.NewClient(environment.Ingress())
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}

	if err := client.SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	handle := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	)
	output, err := handle.Attach(t.Context())
	if err != nil {
		t.Fatalf("await Environment create workflow: %v", err)
	}
	if output.DataVolumeProviderID != "persisted-volume-1" || output.RuntimeID != "runtime-1" {
		t.Fatalf("Environment creation output = %#v", output)
	}
	if got := provider.DataVolumeCreateCount(); got != 1 {
		t.Fatalf("provider mutations = %d, want 1", got)
	}
	if calls, operationID, at := completion.snapshot(); calls != 1 || operationID != input.OperationID || !at.Equal(completedAt) {
		t.Fatalf("completion = calls:%d operation:%q at:%s", calls, operationID, at)
	}
	if calls, operationID, reservation := completion.inventory(); calls != 1 || operationID != input.OperationID ||
		reservation.BackendResourceID != "resource-1" || reservation.WorkspaceID != "workspace-1" ||
		reservation.HomeID != "home-1" || reservation.ServicesID != "services-1" || reservation.CacheID != "cache-1" ||
		reservation.Provider != "fake" || reservation.ProviderID != "fake-volume-environment-1" ||
		string(reservation.Metadata) != `{"availabilityZone":"us-east-1a"}` || !reservation.CreatedAt.Equal(completedAt) {
		t.Fatalf("inventory = calls:%d operation:%q reservation:%#v", calls, operationID, reservation)
	}
	if calls, operationID, reservation := completion.initialRuntime(); calls != 1 || operationID != input.OperationID ||
		reservation.ID != "runtime-1" || reservation.EnvironmentID != input.EnvironmentID || reservation.Sequence != 1 ||
		reservation.Region != input.Region || reservation.AvailabilityZone != input.AvailabilityZone ||
		reservation.RuntimePreset != input.RuntimePreset || reservation.ImageVersion != "image-v1" || !reservation.CreatedAt.Equal(completedAt) {
		t.Fatalf("initial Runtime = calls:%d operation:%q reservation:%#v", calls, operationID, reservation)
	}
	if invocationID := completion.invocation(); invocationID == "" || invocationID == input.OperationID {
		t.Fatalf("actual Restate invocation ID = %q", invocationID)
	}

	reattached, err := handle.Attach(t.Context())
	if err != nil {
		t.Fatalf("reattach completed Environment create workflow: %v", err)
	}
	if reattached != output || provider.DataVolumeCreateCount() != 1 {
		t.Fatalf("reattach changed output or provider state: %#v, mutations:%d", reattached, provider.DataVolumeCreateCount())
	}
	if calls, _, _ := completion.snapshot(); calls != 1 {
		t.Fatalf("completion calls after reattach = %d, want 1", calls)
	}
	if events := completion.eventLog(); len(events) != 4 || events[0] != "record" || events[1] != "inventory" || events[2] != "reserve-runtime" || events[3] != "complete" {
		t.Fatalf("durable store action order = %#v", events)
	}
}

func TestEnvironmentCreateWorkflowDoesNotCompleteAfterInventoryFailure(t *testing.T) {
	dataVolumes := testfixtures.NewProvider()
	completion := &completionFake{}
	store := &inventoryFailureStore{completionFake: completion}
	ids := &workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1"}}
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinition(dataVolumes, store, ids, time.Now, "image-v1"))
	client := workflows.NewClient(environment.Ingress())
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := client.SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inventory failure workflow error = %v, want terminal result", err)
	}
	if calls, _, _ := completion.snapshot(); calls != 0 {
		t.Fatalf("completion calls after inventory failure = %d", calls)
	}
}

func TestEnvironmentCreateWorkflowRejectsDivergedProviderResultBeforeInventory(t *testing.T) {
	dataVolumes := fixedDataVolumeProvider{volume: provider.DataVolume{
		Provider: "aws", ProviderID: "volume-1", EnvironmentID: "environment-other",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
	}}
	store := &completionFake{}
	ids := &workflowIDs{values: []string{"unused"}}
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinition(dataVolumes, store, ids, time.Now, "image-v1"))
	client := workflows.NewClient(environment.Ingress())
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := client.SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	if _, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(t.Context()); err == nil {
		t.Fatal("diverged provider workflow error = nil")
	}
	if calls, _, _ := store.inventory(); calls != 0 {
		t.Fatalf("inventory calls after provider divergence = %d", calls)
	}
	if calls, _, _ := store.snapshot(); calls != 0 {
		t.Fatalf("completion calls after provider divergence = %d", calls)
	}
}

func TestEnvironmentCreateWorkflowTerminatesPermanentProviderFailure(t *testing.T) {
	dataVolumes := &failingDataVolumeProvider{failures: []error{
		provider.NewError(provider.ErrorCodePlacementConflict, "volume belongs to another placement", nil),
	}}
	store := &completionFake{}
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinition(
		dataVolumes, store, &workflowIDs{values: []string{"unused"}}, time.Now, "image-v1",
	))
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(ctx); err == nil {
		t.Fatal("permanent provider failure workflow error = nil")
	}
	if calls := dataVolumes.callCount(); calls != 1 {
		t.Fatalf("permanent provider attempts = %d, want 1", calls)
	}
	if calls, _, _ := store.inventory(); calls != 0 {
		t.Fatalf("inventory calls after permanent provider failure = %d", calls)
	}
}

func TestEnvironmentCreateWorkflowRetriesTransientProviderFailure(t *testing.T) {
	dataVolumes := &failingDataVolumeProvider{
		failures: []error{provider.NewError(provider.ErrorCodeUnavailable, "provider is restarting", nil)},
		volume: provider.DataVolume{
			Provider: "fake", ProviderID: "volume-1", EnvironmentID: "environment-1",
			Region: "us-east-1", AvailabilityZone: "us-east-1a",
		},
	}
	store := &completionFake{}
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinition(
		dataVolumes, store,
		&workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}}, time.Now, "image-v1",
	))
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(ctx)
	if err != nil {
		t.Fatalf("await retried Environment create workflow: %v", err)
	}
	if output.DataVolumeProviderID != "volume-1" || dataVolumes.callCount() != 2 {
		t.Fatalf("retried provider result = %#v, attempts:%d", output, dataVolumes.callCount())
	}
	if calls, _, _ := store.snapshot(); calls != 1 {
		t.Fatalf("completion calls after provider retry = %d", calls)
	}
}

func TestEnvironmentCreateWorkflowRetriesTransientInventoryWithoutRepeatingPriorActions(t *testing.T) {
	dataVolumes := testfixtures.NewProvider()
	completion := &completionFake{}
	store := &transientInventoryStore{completionFake: completion}
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinition(
		dataVolumes, store,
		&workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}}, time.Now, "image-v1",
	))
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(ctx)
	if err != nil {
		t.Fatalf("await retried inventory: %v", err)
	}
	if output.DataVolumeProviderID != "fake-volume-environment-1" || store.attemptCount() != 2 {
		t.Fatalf("retried inventory result = %#v, attempts:%d", output, store.attemptCount())
	}
	if dataVolumes.DataVolumeCreateCount() != 1 {
		t.Fatalf("provider mutations after inventory retry = %d, want 1", dataVolumes.DataVolumeCreateCount())
	}
	if events := completion.eventLog(); len(events) != 4 || events[0] != "record" || events[1] != "inventory" || events[2] != "reserve-runtime" || events[3] != "complete" {
		t.Fatalf("durable actions after inventory retry = %#v", events)
	}
}

func TestEnvironmentCreateWorkflowTerminatesPermanentInitialRuntimeFailure(t *testing.T) {
	dataVolumes := testfixtures.NewProvider()
	completion := &completionFake{}
	store := &runtimeFailureStore{completionFake: completion, failures: []error{permanentActionError{errors.New("Runtime reservation conflicts")}}}
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinition(
		dataVolumes, store,
		&workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}},
		time.Now, "image-v1",
	))
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1",
		AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("permanent Runtime reservation error = %v", err)
	}
	if attempts := store.attemptCount(); attempts != 1 {
		t.Fatalf("permanent Runtime reservation attempts = %d", attempts)
	}
	if calls, _, _ := completion.snapshot(); calls != 0 {
		t.Fatalf("completion calls after Runtime reservation failure = %d", calls)
	}
}

func TestEnvironmentCreateWorkflowRetriesTransientInitialRuntimeFailureWithoutRepeatingPriorActions(t *testing.T) {
	dataVolumes := testfixtures.NewProvider()
	completion := &completionFake{}
	store := &runtimeFailureStore{completionFake: completion, failures: []error{transientActionError{errors.New("database restarting")}}}
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinition(
		dataVolumes, store,
		&workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}},
		time.Now, "image-v1",
	))
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1",
		AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(ctx)
	if err != nil {
		t.Fatalf("await retried Runtime reservation: %v", err)
	}
	if output.RuntimeID != "runtime-1" || store.attemptCount() != 2 {
		t.Fatalf("retried Runtime reservation = %#v attempts:%d", output, store.attemptCount())
	}
	if dataVolumes.DataVolumeCreateCount() != 1 {
		t.Fatalf("Data Volume mutations after Runtime reservation retry = %d", dataVolumes.DataVolumeCreateCount())
	}
}

type completionFake struct {
	mu                  sync.Mutex
	calls               int
	operationID         string
	at                  time.Time
	invocationID        string
	inventoryCalls      int
	runtimeCalls        int
	reservation         domain.EnvironmentStateReservation
	runtimeReservation  domain.RuntimeReservation
	persistedProviderID string
	events              []string
}

func (fake *completionFake) ReserveInitialRuntime(_ context.Context, operationID string, reservation domain.RuntimeReservation) (string, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.runtimeCalls++
	fake.events = append(fake.events, "reserve-runtime")
	fake.operationID, fake.runtimeReservation = operationID, reservation
	return reservation.ID, nil
}

func (fake *completionFake) InventoryEnvironmentState(_ context.Context, operationID string, reservation domain.EnvironmentStateReservation) (string, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.inventoryCalls++
	fake.events = append(fake.events, "inventory")
	fake.operationID, fake.reservation = operationID, reservation
	if fake.persistedProviderID != "" {
		return fake.persistedProviderID, nil
	}
	return reservation.ProviderID, nil
}

func (fake *completionFake) RecordEnvironmentCreateInvocation(_ context.Context, operationID, invocationID string, at time.Time) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.operationID, fake.invocationID, fake.at = operationID, invocationID, at
	fake.events = append(fake.events, "record")
	return nil
}

func (fake *completionFake) CompleteEnvironmentCreation(_ context.Context, operationID string, at time.Time) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.events = append(fake.events, "complete")
	fake.operationID = operationID
	fake.at = at
	return nil
}

func (fake *completionFake) snapshot() (int, string, time.Time) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls, fake.operationID, fake.at
}

func (fake *completionFake) invocation() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.invocationID
}

func (fake *completionFake) inventory() (int, string, domain.EnvironmentStateReservation) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.inventoryCalls, fake.operationID, fake.reservation
}

func (fake *completionFake) initialRuntime() (int, string, domain.RuntimeReservation) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.runtimeCalls, fake.operationID, fake.runtimeReservation
}

func (fake *completionFake) eventLog() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.events...)
}

type inventoryFailureStore struct {
	*completionFake
}

type transientInventoryStore struct {
	*completionFake
	mu       sync.Mutex
	attempts int
}

type runtimeFailureStore struct {
	*completionFake
	mu       sync.Mutex
	attempts int
	failures []error
}

func (store *runtimeFailureStore) ReserveInitialRuntime(ctx context.Context, operationID string, reservation domain.RuntimeReservation) (string, error) {
	store.mu.Lock()
	store.attempts++
	if len(store.failures) != 0 {
		err := store.failures[0]
		store.failures = store.failures[1:]
		store.mu.Unlock()
		return "", err
	}
	store.mu.Unlock()
	return store.completionFake.ReserveInitialRuntime(ctx, operationID, reservation)
}

func (store *runtimeFailureStore) attemptCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.attempts
}

func (store *transientInventoryStore) InventoryEnvironmentState(ctx context.Context, operationID string, reservation domain.EnvironmentStateReservation) (string, error) {
	store.mu.Lock()
	store.attempts++
	attempt := store.attempts
	store.mu.Unlock()
	if attempt == 1 {
		return "", transientActionError{errors.New("database connection reset")}
	}
	return store.completionFake.InventoryEnvironmentState(ctx, operationID, reservation)
}

func (store *transientInventoryStore) attemptCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.attempts
}

func (*inventoryFailureStore) InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (string, error) {
	return "", permanentActionError{errors.New("simulated inventory failure")}
}

type fixedDataVolumeProvider struct {
	volume provider.DataVolume
}

func (fixed fixedDataVolumeProvider) EnsureDataVolume(context.Context, provider.EnsureDataVolumeRequest) (provider.DataVolume, error) {
	return fixed.volume, nil
}

type failingDataVolumeProvider struct {
	mu       sync.Mutex
	calls    int
	failures []error
	volume   provider.DataVolume
}

func (fake *failingDataVolumeProvider) EnsureDataVolume(context.Context, provider.EnsureDataVolumeRequest) (provider.DataVolume, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	if len(fake.failures) != 0 {
		err := fake.failures[0]
		fake.failures = fake.failures[1:]
		return provider.DataVolume{}, err
	}
	return fake.volume, nil
}

func (fake *failingDataVolumeProvider) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

type workflowIDs struct {
	mu     sync.Mutex
	values []string
}

type permanentActionError struct{ error }

func (permanentActionError) Transient() bool { return false }

type transientActionError struct{ error }

func (transientActionError) Transient() bool { return true }

func (ids *workflowIDs) NewID() string {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	value := ids.values[0]
	ids.values = ids.values[1:]
	return value
}
