//go:build !race

// Restate SDK v1.0.0's test HTTP/2 server races in its request-body drain path.
// Keep the real-server workflow test in normal tests; race-test sshai adapters separately.
package workflows_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/application"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

func TestEnvironmentCreateWorkflowRunsDurableProviderAndCompletionActionsOnce(t *testing.T) {
	provider := testfixtures.NewProvider()
	completion := &completionFake{persistedProviderID: "persisted-volume-1"}
	ids := &workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}}
	completedAt := time.Date(2026, time.July, 13, 12, 1, 0, 0, time.UTC)
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(provider, completion, ids, func() time.Time { return completedAt }, "image-v1"))
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

func TestEnvironmentCreateWorkflowResolvesAndPinsCapsuleStateAfterRuntime(t *testing.T) {
	provider := testfixtures.NewProvider()
	completion := &completionFake{persistedProviderID: "persisted-volume-1"}
	capsule := &capsuleStateFake{
		completionFake: completion,
		state: workflows.EnvironmentCapsuleState{
			CapsuleLock:   materializationLock(),
			UpgradePolicy: domain.UpgradeNotify,
			ApplyResults:  []profile.ProfileMaterializationResult{successfulMaterializationResult("config:editor")},
		},
	}
	ids := &workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}}
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(provider, capsule, ids, time.Now, "image-v1"))
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	if _, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID).Attach(t.Context()); err != nil {
		t.Fatalf("await Environment create workflow: %v", err)
	}
	if capsule.resolveCalls != 1 || capsule.persistCalls != 1 {
		t.Fatalf("Capsule state calls = resolve:%d persist:%d", capsule.resolveCalls, capsule.persistCalls)
	}
	if capsule.persistedState.CapsuleLock.ID != "lock-1" || capsule.persistedState.UpgradePolicy != domain.UpgradeNotify || len(capsule.persistedState.ApplyResults) != 1 {
		t.Fatalf("persisted Capsule state = %#v", capsule.persistedState)
	}
	if events := capsule.eventLog(); len(events) != 6 || events[2] != "reserve-runtime" || events[3] != "resolve-profile-version" || events[4] != "persist-capsule-state" || events[5] != "complete" {
		t.Fatalf("Capsule state action order = %#v", events)
	}
}

func TestEnvironmentCreateWorkflowTreatsCapsuleLockConflictAsTerminal(t *testing.T) {
	provider := testfixtures.NewProvider()
	capsule := &capsuleStateFake{
		completionFake: &completionFake{persistedProviderID: "persisted-volume-1"},
		state: workflows.EnvironmentCapsuleState{
			CapsuleLock: domain.CapsuleLockSnapshot{
				ID: "lock-conflict", EnvironmentID: "environment-1", ProfileVersionID: "version-1",
			},
			UpgradePolicy: domain.UpgradeManual,
		},
		persistErr: dbstore.ErrCapsuleLockConflict,
	}
	ids := &workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}}
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(provider, capsule, ids, time.Now, "image-v1"))
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-conflict", EnvironmentID: "environment-1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Capsule Lock conflict workflow error = %v, want terminal failure", err)
	}
	if capsule.persistCalls != 1 {
		t.Fatalf("Capsule Lock conflict persistence calls = %d, want one terminal attempt", capsule.persistCalls)
	}
}

func TestEnvironmentCreateWorkflowDoesNotResolveCapsuleStateAfterProviderValidationFailure(t *testing.T) {
	dataVolumes := fixedDataVolumeProvider{volume: provider.DataVolume{
		Provider: "aws", ProviderID: "volume-1", EnvironmentID: "environment-other", Region: "us-east-1", AvailabilityZone: "us-east-1a",
	}}
	capsule := &capsuleStateFake{completionFake: &completionFake{}}
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(dataVolumes, capsule, &workflowIDs{values: []string{"unused"}}, time.Now, "image-v1"))
	input := application.EnvironmentCreateWorkflowInput{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	if _, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID).Attach(t.Context()); err == nil {
		t.Fatal("provider validation failure completed successfully")
	}
	if capsule.resolveCalls != 0 || capsule.persistCalls != 0 {
		t.Fatalf("Capsule state calls after provider validation failure = resolve:%d persist:%d", capsule.resolveCalls, capsule.persistCalls)
	}
}

func TestInstalledMaterializationsFromApplyResultsPreservesCacheIdentity(t *testing.T) {
	results := []profile.ProfileMaterializationResult{{
		ID: "editor", LockID: "lock-1", LockDigest: "sha256:lock", CapsuleDigest: "sha256:capsule", ComponentID: "config:editor",
		ComponentDigest: "sha256:component", Adapter: "file", AdapterVersion: "v1", TargetAgentVersion: "agent-1",
		Scope: domain.ScopeUser, NonSecretOverridesDigest: "sha256:overrides", SecretVersionIdentifiers: []string{"secret-1"}, EffectiveCacheKey: "sha256:key",
		Mode: "managed", Root: "home", Target: ".config/editor", Selector: "$", LastAppliedDigest: "sha256:last", ObservedDigest: "sha256:observed", CredentialRequirementDigest: "sha256:requirements",
	}}
	installed := workflows.InstalledMaterializationsFromApplyResults(results)
	if len(installed) != 1 || installed[0].ComponentID != "config:editor" || installed[0].EffectiveCacheKey != "sha256:key" || installed[0].SecretVersionIdentifiers[0] != "secret-1" {
		t.Fatalf("installed materializations = %#v", installed)
	}
}

func TestEnvironmentCreateWorkflowDoesNotCompleteAfterInventoryFailure(t *testing.T) {
	dataVolumes := testfixtures.NewProvider()
	completion := &completionFake{}
	store := &inventoryFailureStore{completionFake: completion}
	ids := &workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}}
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(dataVolumes, store, ids, time.Now, "image-v1"))
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
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(dataVolumes, store, ids, time.Now, "image-v1"))
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
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(
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
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(
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
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(
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
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(
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
	environment := testfixtures.StartRestate(t, environmentCreateDefinition(
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

func TestEnvironmentCreateWorkflowCompletesStepsFourThroughEleven(t *testing.T) {
	harness := newEnvironmentCreateStepsHarness()
	result := harness.run(t, "happy")
	if result.err != nil {
		t.Fatalf("complete Environment create: %v", result.err)
	}
	if result.output.RuntimeID != "runtime-1" || result.output.DataVolumeProviderID != "volume-1" {
		t.Fatalf("Environment create output = %#v", result.output)
	}
	if harness.provider.ensureRuntimeCalls != 1 || harness.provider.attachRuntimeCalls != 1 || harness.actions.completeCalls != 1 {
		t.Fatalf("provider/completion calls = allocate:%d attach:%d complete:%d", harness.provider.ensureRuntimeCalls, harness.provider.attachRuntimeCalls, harness.actions.completeCalls)
	}
	if got := harness.guest.calls; strings.Join(got, ",") != "restore-identity,readiness,ssh-keys,seed,materialize,credentials,toolchain" {
		t.Fatalf("guest step order = %#v", got)
	}
	if status := harness.actions.lastRuntime.Status; status != domain.RuntimeReady {
		t.Fatalf("final Runtime status = %q, want %q", status, domain.RuntimeReady)
	}
	if len(harness.actions.outcomes) != 0 {
		t.Fatalf("successful create recorded terminal outcomes = %#v", harness.actions.outcomes)
	}
}

func TestEnvironmentCreateWorkflowAttachmentFailureKeepsInventoriedRuntime(t *testing.T) {
	harness := newEnvironmentCreateStepsHarness()
	harness.provider.attachRuntimeErr = provider.NewError(provider.ErrorCodePlacementConflict, "attachment failed", nil)
	result := harness.run(t, "attachment-failure")
	if result.err == nil {
		t.Fatal("attachment failure completed successfully")
	}
	if harness.actions.lastRuntime.Status != domain.RuntimeError || harness.actions.lastRuntime.ProviderInstanceRef == nil || *harness.actions.lastRuntime.ProviderInstanceRef != "instance-1" {
		t.Fatalf("Runtime after attachment failure = %#v", harness.actions.lastRuntime)
	}
	if harness.provider.ensureRuntimeCalls != 1 || harness.provider.attachRuntimeCalls != 1 || harness.provider.dataVolumeDeletes != 0 {
		t.Fatalf("provider mutations = allocate:%d attach:%d Data Volume deletes:%d", harness.provider.ensureRuntimeCalls, harness.provider.attachRuntimeCalls, harness.provider.dataVolumeDeletes)
	}
}

func TestEnvironmentCreateWorkflowProvisionFailureFinalizesWithoutDeletingDataVolume(t *testing.T) {
	harness := newEnvironmentCreateStepsHarness()
	harness.provider.ensureRuntimeErr = provider.NewError(provider.ErrorCodePlacementConflict, "placement conflict", nil)
	result := harness.run(t, "provision-failure")
	if result.err == nil {
		t.Fatal("provision failure completed successfully")
	}
	if harness.provider.dataVolumeDeletes != 0 {
		t.Fatalf("Data Volume deletions = %d, want zero", harness.provider.dataVolumeDeletes)
	}
	if len(harness.actions.outcomes) != 1 || harness.actions.outcomes[0].Kind != workflows.EnvironmentCreateOutcomeFailed {
		t.Fatalf("recorded outcomes = %#v", harness.actions.outcomes)
	}
	if harness.actions.lastRuntime.Status != domain.RuntimeError || harness.actions.lastRuntime.ProviderInstanceRef != nil {
		t.Fatalf("failed provision Runtime = %#v", harness.actions.lastRuntime)
	}
}

func TestEnvironmentCreateWorkflowGuestReadinessTimeoutIsTerminal(t *testing.T) {
	harness := newEnvironmentCreateStepsHarness()
	harness.guest.readiness = workflows.RuntimeGuestReadiness{PrivateIPv4: "10.0.0.8"}
	harness.dependencies.GuestPollInterval = 5 * time.Millisecond
	harness.dependencies.GuestPollTimeout = 20 * time.Millisecond
	result := harness.run(t, "guest-timeout")
	if result.err == nil {
		t.Fatal("guest readiness timeout completed successfully")
	}
	if len(harness.actions.outcomes) != 1 || harness.actions.outcomes[0].Code != workflows.GuestNotReady {
		t.Fatalf("guest timeout outcome = %#v", harness.actions.outcomes)
	}
	if harness.actions.lastRuntime.Status != domain.RuntimeError || harness.provider.dataVolumeDeletes != 0 {
		t.Fatalf("guest timeout state = Runtime:%#v deletes:%d", harness.actions.lastRuntime, harness.provider.dataVolumeDeletes)
	}
}

func TestEnvironmentCreateWorkflowSeedFailureFinalizesOperation(t *testing.T) {
	harness := newEnvironmentCreateStepsHarness()
	harness.guest.seedErr = permanentActionError{errors.New("invalid Project Seed archive")}
	result := harness.run(t, "seed-failure")
	if result.err == nil {
		t.Fatal("Project Seed failure completed successfully")
	}
	if len(harness.actions.outcomes) != 1 || harness.actions.outcomes[0].Code != "PROJECT_SEED_INVALID" {
		t.Fatalf("Project Seed failure outcome = %#v", harness.actions.outcomes)
	}
	if harness.actions.persistCapsuleCalls != 0 || harness.provider.dataVolumeDeletes != 0 {
		t.Fatalf("post-seed mutations = Capsule:%d deletes:%d", harness.actions.persistCapsuleCalls, harness.provider.dataVolumeDeletes)
	}
}

func TestEnvironmentCreateWorkflowGuestMutationRetriesUseStableEnsureIdentity(t *testing.T) {
	harness := newEnvironmentCreateStepsHarness()
	harness.actions.capsuleState.CapsuleLock = materializationLock()
	harness.guest.materializations = []profile.ProfileMaterializationResult{successfulMaterializationResult("config:editor")}
	harness.guest.seedResponseLoss = true
	harness.guest.materializationResponseLoss = true
	result := harness.run(t, "guest-response-loss")
	if result.err != nil {
		t.Fatalf("create after ambiguous guest responses: %v", result.err)
	}
	if harness.guest.seedAttempts != 2 || harness.guest.seedMutations != 1 || harness.guest.materializationAttempts != 2 || harness.guest.materializationMutations != 1 {
		t.Fatalf("guest attempts/mutations = seed:%d/%d materialization:%d/%d", harness.guest.seedAttempts, harness.guest.seedMutations, harness.guest.materializationAttempts, harness.guest.materializationMutations)
	}
}

func TestEnvironmentCreateWorkflowRecordsRequiresInputOutcome(t *testing.T) {
	harness := newEnvironmentCreateStepsHarness()
	harness.guest.binding = workflows.EnvironmentCredentialBindingOutcome{
		Status: workflows.EnvironmentCredentialsRequireInput, Code: workflows.CredentialRequired,
		Message: "repository token is required",
	}
	result := harness.run(t, "requires-input")
	if result.err == nil {
		t.Fatal("requires_input create completed successfully")
	}
	if len(harness.actions.outcomes) != 1 || harness.actions.outcomes[0].Kind != workflows.EnvironmentCreateOutcomeRequiresInput || harness.actions.outcomes[0].Code != workflows.CredentialRequired {
		t.Fatalf("requires_input outcome = %#v", harness.actions.outcomes)
	}
	if harness.actions.completeCalls != 0 || harness.guest.called("toolchain") {
		t.Fatalf("requires_input continued = complete:%d guest:%#v", harness.actions.completeCalls, harness.guest.calls)
	}
}

func TestEnvironmentCreateWorkflowRecordsMaterializationResultsFromGuest(t *testing.T) {
	harness := newEnvironmentCreateStepsHarness()
	harness.actions.capsuleState.CapsuleLock = materializationLock()
	harness.guest.materializations = []profile.ProfileMaterializationResult{successfulMaterializationResult("config:editor")}
	result := harness.run(t, "materialization")
	if result.err != nil {
		t.Fatalf("materialized create: %v", result.err)
	}
	state := harness.actions.persistedCapsule
	if len(state.ApplyResults) != 1 || len(state.Materializations) != 1 || state.Materializations[0].EffectiveCacheKey != "sha256:key" {
		t.Fatalf("persisted materialization state = %#v", state)
	}
}

func TestEnvironmentCreateWorkflowRejectsIncompleteOrNonConvergedMaterialization(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*environmentCreateStepsHarness)
	}{
		{name: "incomplete", prepare: func(harness *environmentCreateStepsHarness) {}},
		{name: "duplicate", prepare: func(harness *environmentCreateStepsHarness) {
			lock := materializationLock()
			lock.ResolvedComponents["config:shell"] = domain.ResolvedComponent{ID: "config:shell", CapsuleDigest: "sha256:capsule", ComponentDigest: "sha256:shell", Scope: domain.ScopeUser}
			harness.actions.capsuleState.CapsuleLock = lock
			result := successfulMaterializationResult("config:editor")
			harness.guest.materializations = []profile.ProfileMaterializationResult{result, result}
		}},
		{name: "non-converged", prepare: func(harness *environmentCreateStepsHarness) {
			result := successfulMaterializationResult("config:editor")
			result.Operation = profile.OperationConflict
			harness.guest.materializations = []profile.ProfileMaterializationResult{result}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newEnvironmentCreateStepsHarness()
			harness.actions.capsuleState.CapsuleLock = materializationLock()
			test.prepare(harness)
			result := harness.run(t, "materialization-"+test.name)
			if result.err == nil {
				t.Fatal("invalid materialization completed successfully")
			}
			if harness.actions.persistCapsuleCalls != 0 || len(harness.actions.outcomes) != 1 || harness.actions.outcomes[0].Code != "PROFILE_CONFLICT" {
				t.Fatalf("invalid materialization persistence/outcome = %d/%#v", harness.actions.persistCapsuleCalls, harness.actions.outcomes)
			}
		})
	}
}

func materializationLock() domain.CapsuleLockSnapshot {
	return domain.CapsuleLockSnapshot{
		ID: "lock-1", EnvironmentID: "environment-1", ProfileVersionID: "version-1", Digest: "sha256:lock",
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {ID: "config:editor", CapsuleDigest: "sha256:capsule", ComponentDigest: "sha256:component", Scope: domain.ScopeUser},
		},
	}
}

func successfulMaterializationResult(componentID string) profile.ProfileMaterializationResult {
	return profile.ProfileMaterializationResult{
		ID: "editor", LockID: "lock-1", LockDigest: "sha256:lock", CapsuleDigest: "sha256:capsule",
		ComponentID: componentID, ComponentDigest: "sha256:component", Adapter: "claude",
		AdapterVersion: "v1", Scope: domain.ScopeUser, Mode: profile.MaterializationManaged,
		Root: profile.MaterializationHome, Target: ".config/editor", Selector: "$", EffectiveCacheKey: "sha256:key",
		DesiredDigest: "sha256:desired", LastAppliedDigest: "sha256:desired", ObservedDigest: "sha256:desired", Operation: profile.OperationCreate,
	}
}

type environmentCreateStepsHarness struct {
	provider     *environmentCreateStepsProvider
	actions      *environmentCreateStepsActions
	guest        *environmentCreateStepsGuest
	dependencies workflows.EnvironmentCreateDependencies
}

func newEnvironmentCreateStepsHarness() *environmentCreateStepsHarness {
	providerFake := &environmentCreateStepsProvider{}
	actions := &environmentCreateStepsActions{capsuleState: workflows.EnvironmentCapsuleState{
		CapsuleLock:   domain.CapsuleLockSnapshot{ID: "lock-1", EnvironmentID: "environment-1", ProfileVersionID: "version-1"},
		UpgradePolicy: domain.UpgradeManual,
	}}
	guest := &environmentCreateStepsGuest{
		readiness: workflows.RuntimeGuestReadiness{BootID: "boot-1", PrivateIPv4: "10.0.0.8", DataMounted: true},
		binding:   workflows.EnvironmentCredentialBindingOutcome{Status: workflows.EnvironmentCredentialsNotRequired},
	}
	harness := &environmentCreateStepsHarness{provider: providerFake, actions: actions, guest: guest}
	harness.dependencies = workflows.EnvironmentCreateDependencies{
		Provider: providerFake, Actions: actions, Capsules: actions,
		SSHIdentity: guest, GuestReadiness: guest, SSHKeys: guest, ProjectSeed: guest, Materializer: guest,
		Credentials: guest, Toolchain: guest,
		IDs: &workflowIDs{values: []string{"resource-1", "workspace-1", "home-1", "services-1", "cache-1", "runtime-1"}},
		Now: time.Now, ImageVersion: "image-v1", ProviderPollInterval: time.Millisecond, ProviderPollTimeout: time.Second,
		GuestPollInterval: time.Millisecond, GuestPollTimeout: time.Second,
	}
	return harness
}

type environmentCreateRunResult struct {
	output workflows.EnvironmentCreateOutput
	err    error
}

func (harness *environmentCreateStepsHarness) run(t *testing.T, suffix string) environmentCreateRunResult {
	t.Helper()
	environment := testfixtures.StartRestate(t, workflows.EnvironmentCreateDefinitionWithDependencies(harness.dependencies))
	input := domain.EnvironmentCreateDispatch{
		OperationID: "operation-" + suffix, EnvironmentID: "environment-1",
		Region: "eu-central-1", AvailabilityZone: "eu-central-1a", RuntimePreset: "cpu2-mem8",
	}
	if err := workflows.NewClient(environment.Ingress()).SendEnvironmentCreate(t.Context(), input); err != nil {
		t.Fatalf("submit Environment create workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output, err := ingress.WorkflowHandle[workflows.EnvironmentCreateOutput](
		environment.Ingress(), workflows.EnvironmentCreateService, input.OperationID,
	).Attach(ctx)
	return environmentCreateRunResult{output: output, err: err}
}

type environmentCreateStepsProvider struct {
	mu                 sync.Mutex
	runtime            provider.Runtime
	ensureRuntimeCalls int
	ensureRuntimeErr   error
	attachRuntimeCalls int
	attachRuntimeErr   error
	dataVolumeDeletes  int
}

func (fake *environmentCreateStepsProvider) EnsureDataVolume(_ context.Context, request provider.EnsureDataVolumeRequest) (provider.DataVolume, error) {
	return provider.DataVolume{Provider: "fake", ProviderID: "volume-1", EnvironmentID: request.EnvironmentID, Region: request.Region, AvailabilityZone: request.AvailabilityZone}, nil
}

func (fake *environmentCreateStepsProvider) EnsureRuntime(_ context.Context, request provider.EnsureRuntimeRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.ensureRuntimeCalls++
	if fake.ensureRuntimeErr != nil {
		return provider.Runtime{}, fake.ensureRuntimeErr
	}
	if fake.runtime.ProviderID == "" {
		fake.runtime = provider.Runtime{RuntimeSpec: request.RuntimeSpec, ProviderID: "instance-1", State: provider.RuntimeStatePending}
	}
	return fake.runtime, nil
}

func (fake *environmentCreateStepsProvider) EnsureRuntimeDataVolumeAttachment(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.attachRuntimeCalls++
	if fake.attachRuntimeErr != nil {
		return provider.Runtime{}, fake.attachRuntimeErr
	}
	if fake.runtime.RuntimeID != request.RuntimeID || fake.runtime.ProviderID != request.ProviderID {
		return provider.Runtime{}, errors.New("attach Runtime identity does not match allocation")
	}
	return fake.runtime, nil
}

func (fake *environmentCreateStepsProvider) ObserveRuntime(context.Context, provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.runtime.State == provider.RuntimeStatePending {
		fake.runtime.State = provider.RuntimeStateRunning
		fake.runtime.PrivateIPv4 = "10.0.0.8"
	}
	return fake.runtime, nil
}

func (fake *environmentCreateStepsProvider) StartRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return fake.ObserveRuntime(ctx, request)
}

func (fake *environmentCreateStepsProvider) StopRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return fake.ObserveRuntime(ctx, request)
}

func (fake *environmentCreateStepsProvider) RetireRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return fake.ObserveRuntime(ctx, request)
}

type environmentCreateStepsActions struct {
	mu                  sync.Mutex
	runtimeReservation  domain.RuntimeReservation
	lastRuntime         domain.RuntimeSnapshot
	outcomes            []workflows.EnvironmentCreateOperationOutcome
	completeCalls       int
	capsuleState        workflows.EnvironmentCapsuleState
	persistedCapsule    workflows.EnvironmentCapsuleState
	persistCapsuleCalls int
}

func (*environmentCreateStepsActions) RecordEnvironmentCreateInvocation(_ context.Context, _ string, _ string, _ time.Time) (workflows.EnvironmentCreateInvocation, error) {
	return workflows.EnvironmentCreateInvocation{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", ProjectSeedID: "project-seed-1",
	}, nil
}

func (*environmentCreateStepsActions) InventoryEnvironmentState(_ context.Context, _ string, reservation domain.EnvironmentStateReservation) (string, error) {
	return reservation.ProviderID, nil
}

func (actions *environmentCreateStepsActions) ReserveInitialRuntime(_ context.Context, _ string, reservation domain.RuntimeReservation) (string, error) {
	actions.runtimeReservation = reservation
	return reservation.ID, nil
}

func (actions *environmentCreateStepsActions) PersistEnvironmentCreateRuntimeTransition(_ context.Context, _ string, _ int64, next domain.RuntimeSnapshot) error {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	actions.lastRuntime = next
	return nil
}

func (actions *environmentCreateStepsActions) RecordEnvironmentCreateOutcome(_ context.Context, _ string, outcome workflows.EnvironmentCreateOperationOutcome, _ time.Time) error {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	actions.outcomes = append(actions.outcomes, outcome)
	return nil
}

func (actions *environmentCreateStepsActions) CompleteEnvironmentCreation(context.Context, string, time.Time) error {
	actions.completeCalls++
	return nil
}

func (actions *environmentCreateStepsActions) ResolvePinnedProfileVersion(context.Context, string, time.Time) (workflows.EnvironmentCapsuleState, error) {
	return actions.capsuleState, nil
}

func (actions *environmentCreateStepsActions) PersistEnvironmentCapsuleState(_ context.Context, _ string, state workflows.EnvironmentCapsuleState) error {
	actions.persistCapsuleCalls++
	actions.persistedCapsule = state
	return nil
}

type environmentCreateStepsGuest struct {
	mu                          sync.Mutex
	calls                       []string
	readiness                   workflows.RuntimeGuestReadiness
	seedErr                     error
	seedResponseLoss            bool
	seedAttempts                int
	seedMutations               int
	seedEnsureIdentity          string
	materializations            []profile.ProfileMaterializationResult
	materializationResponseLoss bool
	materializationAttempts     int
	materializationMutations    int
	materializationEnsureID     string
	binding                     workflows.EnvironmentCredentialBindingOutcome
}

func (fake *environmentCreateStepsGuest) record(call string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, call)
}

func (fake *environmentCreateStepsGuest) called(call string) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, candidate := range fake.calls {
		if candidate == call {
			return true
		}
	}
	return false
}

func (fake *environmentCreateStepsGuest) RestoreEnvironmentSSHIdentity(context.Context, workflows.EnvironmentCreateGuestRequest) error {
	fake.record("restore-identity")
	return nil
}

func (fake *environmentCreateStepsGuest) WaitForRuntimeReady(context.Context, workflows.RuntimeGuestReadinessRequest) (workflows.RuntimeGuestReadiness, error) {
	fake.record("readiness")
	return fake.readiness, nil
}

func (fake *environmentCreateStepsGuest) ReconcileRuntimeSSHKeys(_ context.Context, request workflows.RuntimeGuestReadinessRequest) error {
	if request.OwnerUserID != "user-1" {
		return fmt.Errorf("reconcile SSH Keys owner = %q", request.OwnerUserID)
	}
	fake.record("ssh-keys")
	return nil
}

func (fake *environmentCreateStepsGuest) EnsureEnvironmentProjectSeedApplied(_ context.Context, request workflows.EnvironmentProjectSeedRequest) error {
	if request.Guest.OwnerUserID != "user-1" || request.ProjectSeedID != "project-seed-1" {
		return fmt.Errorf("Project Seed request = %#v", request)
	}
	fake.record("seed")
	if fake.seedResponseLoss {
		fake.mu.Lock()
		identity := request.Guest.OperationID + ":" + request.ProjectSeedID
		if fake.seedEnsureIdentity == "" {
			fake.seedEnsureIdentity = identity
		}
		identityChanged := fake.seedEnsureIdentity != identity
		fake.seedAttempts++
		firstAttempt := fake.seedAttempts == 1
		if firstAttempt {
			fake.seedMutations++
		}
		fake.mu.Unlock()
		if identityChanged {
			return permanentActionError{errors.New("Project Seed ensure identity changed across retry")}
		}
		if firstAttempt {
			return transientActionError{errors.New("Project Seed response lost after apply")}
		}
	}
	return fake.seedErr
}

func (fake *environmentCreateStepsGuest) EnsureEnvironmentCapsuleMaterialized(_ context.Context, request workflows.EnvironmentCapsuleMaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	fake.record("materialize")
	if fake.materializationResponseLoss {
		fake.mu.Lock()
		identity := request.Guest.OperationID + ":" + request.State.CapsuleLock.ID + ":" + request.State.CapsuleLock.Digest
		if fake.materializationEnsureID == "" {
			fake.materializationEnsureID = identity
		}
		identityChanged := fake.materializationEnsureID != identity
		fake.materializationAttempts++
		firstAttempt := fake.materializationAttempts == 1
		if firstAttempt {
			fake.materializationMutations++
		}
		fake.mu.Unlock()
		if identityChanged {
			return nil, permanentActionError{errors.New("materialization ensure identity changed across retry")}
		}
		if firstAttempt {
			return nil, transientActionError{errors.New("materialization response lost after apply")}
		}
	}
	return fake.materializations, nil
}

func (fake *environmentCreateStepsGuest) BindEnvironmentCredentials(context.Context, workflows.EnvironmentCreateGuestRequest) (workflows.EnvironmentCredentialBindingOutcome, error) {
	fake.record("credentials")
	return fake.binding, nil
}

func (fake *environmentCreateStepsGuest) ValidateEnvironmentToolchain(context.Context, workflows.EnvironmentCreateGuestRequest) error {
	fake.record("toolchain")
	return nil
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

func (fake *completionFake) RecordEnvironmentCreateInvocation(_ context.Context, operationID, invocationID string, at time.Time) (workflows.EnvironmentCreateInvocation, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.operationID, fake.invocationID, fake.at = operationID, invocationID, at
	fake.events = append(fake.events, "record")
	return workflows.EnvironmentCreateInvocation{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", ProjectSeedID: "project-seed-1",
	}, nil
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

type capsuleStateFake struct {
	*completionFake
	state          workflows.EnvironmentCapsuleState
	persistErr     error
	resolveCalls   int
	persistCalls   int
	persistedState workflows.EnvironmentCapsuleState
}

func (fake *capsuleStateFake) ResolvePinnedProfileVersion(context.Context, string, time.Time) (workflows.EnvironmentCapsuleState, error) {
	fake.resolveCalls++
	fake.completionFake.events = append(fake.completionFake.events, "resolve-profile-version")
	return fake.state, nil
}

func (fake *capsuleStateFake) PersistEnvironmentCapsuleState(_ context.Context, _ string, state workflows.EnvironmentCapsuleState) error {
	fake.persistCalls++
	fake.persistedState = state
	fake.completionFake.events = append(fake.completionFake.events, "persist-capsule-state")
	return fake.persistErr
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
	if len(ids.values) == 0 {
		panic("workflowIDs exhausted: reserve-creation-identities requires six deterministic IDs")
	}
	value := ids.values[0]
	ids.values = ids.values[1:]
	return value
}

type legacyEnvironmentCreationActions interface {
	RecordEnvironmentCreateInvocation(context.Context, string, string, time.Time) (workflows.EnvironmentCreateInvocation, error)
	InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (string, error)
	ReserveInitialRuntime(context.Context, string, domain.RuntimeReservation) (string, error)
	CompleteEnvironmentCreation(context.Context, string, time.Time) error
}

func environmentCreateDefinition(dataVolumes provider.DataVolumeProvider, actions legacyEnvironmentCreationActions, ids workflows.IDGenerator, now func() time.Time, imageVersion string) restate.ServiceDefinition {
	providerAdapter, ok := dataVolumes.(workflows.EnvironmentCreateProvider)
	if !ok {
		providerAdapter = &environmentCreateProviderAdapter{DataVolumeProvider: dataVolumes, runtimes: make(map[string]provider.Runtime)}
	}
	wrapped := &environmentCreateActionsAdapter{legacyEnvironmentCreationActions: actions}
	capsules, ok := actions.(workflows.EnvironmentCreationCapsuleActions)
	if !ok {
		capsules = testEnvironmentCapsuleActions{}
	}
	guest := testEnvironmentGuest{}
	return workflows.EnvironmentCreateDefinitionWithDependencies(workflows.EnvironmentCreateDependencies{
		Provider: providerAdapter, Actions: wrapped, Capsules: capsules,
		SSHIdentity: guest, GuestReadiness: guest, SSHKeys: guest, ProjectSeed: guest, Materializer: guest,
		Credentials: workflows.NoProjectCredentialBinder{}, Toolchain: guest,
		IDs: ids, Now: now, ImageVersion: imageVersion,
		ProviderPollInterval: time.Millisecond, ProviderPollTimeout: time.Second,
		GuestPollInterval: time.Millisecond, GuestPollTimeout: time.Second,
	})
}

type environmentCreateActionsAdapter struct {
	legacyEnvironmentCreationActions
	mu       sync.Mutex
	runtimes map[string]domain.RuntimeSnapshot
	outcomes []workflows.EnvironmentCreateOperationOutcome
}

func (actions *environmentCreateActionsAdapter) PersistEnvironmentCreateRuntimeTransition(_ context.Context, _ string, _ int64, next domain.RuntimeSnapshot) error {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	if actions.runtimes == nil {
		actions.runtimes = make(map[string]domain.RuntimeSnapshot)
	}
	actions.runtimes[next.ID] = next
	return nil
}

func (actions *environmentCreateActionsAdapter) RecordEnvironmentCreateOutcome(_ context.Context, _ string, outcome workflows.EnvironmentCreateOperationOutcome, _ time.Time) error {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	actions.outcomes = append(actions.outcomes, outcome)
	return nil
}

type environmentCreateProviderAdapter struct {
	provider.DataVolumeProvider
	mu       sync.Mutex
	runtimes map[string]provider.Runtime
}

func (fake *environmentCreateProviderAdapter) EnsureRuntime(_ context.Context, request provider.EnsureRuntimeRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if existing, ok := fake.runtimes[request.RuntimeID]; ok {
		return existing, nil
	}
	runtime := provider.Runtime{RuntimeSpec: request.RuntimeSpec, ProviderID: "instance-" + request.RuntimeID, PrivateIPv4: "10.0.0.8", State: provider.RuntimeStateRunning}
	fake.runtimes[request.RuntimeID] = runtime
	return runtime, nil
}

func (fake *environmentCreateProviderAdapter) EnsureRuntimeDataVolumeAttachment(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return fake.ObserveRuntime(ctx, request)
}

func (fake *environmentCreateProviderAdapter) ObserveRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.runtimes[request.RuntimeID], nil
}

func (fake *environmentCreateProviderAdapter) StartRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return fake.ObserveRuntime(ctx, request)
}

func (fake *environmentCreateProviderAdapter) StopRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return fake.ObserveRuntime(ctx, request)
}

func (fake *environmentCreateProviderAdapter) RetireRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return fake.ObserveRuntime(ctx, request)
}

type testEnvironmentCapsuleActions struct{}

func (testEnvironmentCapsuleActions) ResolvePinnedProfileVersion(_ context.Context, operationID string, _ time.Time) (workflows.EnvironmentCapsuleState, error) {
	environmentID := strings.Replace(operationID, "operation", "environment", 1)
	return workflows.EnvironmentCapsuleState{CapsuleLock: domain.CapsuleLockSnapshot{ID: "lock-" + operationID, EnvironmentID: environmentID, ProfileVersionID: "version-1"}, UpgradePolicy: domain.UpgradeManual}, nil
}

func (testEnvironmentCapsuleActions) PersistEnvironmentCapsuleState(context.Context, string, workflows.EnvironmentCapsuleState) error {
	return nil
}

type testEnvironmentGuest struct{}

func (testEnvironmentGuest) RestoreEnvironmentSSHIdentity(context.Context, workflows.EnvironmentCreateGuestRequest) error {
	return nil
}

func (testEnvironmentGuest) WaitForRuntimeReady(_ context.Context, request workflows.RuntimeGuestReadinessRequest) (workflows.RuntimeGuestReadiness, error) {
	return workflows.RuntimeGuestReadiness{BootID: "boot-1", PrivateIPv4: request.PrivateIPv4, DataMounted: true}, nil
}

func (testEnvironmentGuest) ReconcileRuntimeSSHKeys(context.Context, workflows.RuntimeGuestReadinessRequest) error {
	return nil
}

func (testEnvironmentGuest) EnsureEnvironmentProjectSeedApplied(context.Context, workflows.EnvironmentProjectSeedRequest) error {
	return nil
}

func (testEnvironmentGuest) EnsureEnvironmentCapsuleMaterialized(_ context.Context, request workflows.EnvironmentCapsuleMaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	return request.State.ApplyResults, nil
}

func (testEnvironmentGuest) ValidateEnvironmentToolchain(context.Context, workflows.EnvironmentCreateGuestRequest) error {
	return nil
}
