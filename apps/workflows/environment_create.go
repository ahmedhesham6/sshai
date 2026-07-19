package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

const (
	EnvironmentCreateService = "EnvironmentCreate"
	RunHandler               = "Run"
	EnvironmentCreateFailed  = "ENVIRONMENT_CREATE_FAILED"
	CredentialRequired       = "CREDENTIAL_REQUIRED"
)

type EnvironmentCreationActions interface {
	RecordEnvironmentCreateInvocation(context.Context, string, string, time.Time) (EnvironmentCreateInvocation, error)
	InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (string, error)
	ReserveInitialRuntime(context.Context, string, domain.RuntimeReservation) (string, error)
	PersistEnvironmentCreateRuntimeTransition(context.Context, string, int64, domain.RuntimeSnapshot) error
	RecordEnvironmentCreateOutcome(context.Context, string, EnvironmentCreateOperationOutcome, time.Time) error
	CompleteEnvironmentCreation(context.Context, string, time.Time) error
}

// EnvironmentCreateInvocation is the authoritative owner and Project Seed
// state loaded while claiming an environment.create Operation. Guest-side
// requests must use this state rather than trusting dispatch input.
type EnvironmentCreateInvocation struct {
	OwnerUserID   string `json:"ownerUserId"`
	EnvironmentID string `json:"environmentId"`
	ProjectSeedID string `json:"projectSeedId"`
}

type EnvironmentCreateOutcomeKind string

const (
	EnvironmentCreateOutcomeFailed        EnvironmentCreateOutcomeKind = "failed"
	EnvironmentCreateOutcomeRequiresInput EnvironmentCreateOutcomeKind = "requires_input"
)

type EnvironmentCreateOperationOutcome struct {
	Kind    EnvironmentCreateOutcomeKind `json:"kind"`
	Code    string                       `json:"code"`
	Message string                       `json:"message"`
}

// EnvironmentCapsuleState is the result of the existing profile.resolve
// pathway and the guest apply-results seam. The workflow persists it only
// after runtime validation has completed.
type EnvironmentCapsuleState struct {
	CapsuleLock      domain.CapsuleLockSnapshot             `json:"capsuleLock"`
	UpgradePolicy    domain.UpgradePolicy                   `json:"upgradePolicy"`
	ApplyResults     []profile.ProfileMaterializationResult `json:"applyResults,omitempty"`
	Materializations []profile.InstalledMaterialization     `json:"materializations,omitempty"`
}

type EnvironmentCreationCapsuleActions interface {
	ResolvePinnedProfileVersion(context.Context, string, time.Time) (EnvironmentCapsuleState, error)
	PersistEnvironmentCapsuleState(context.Context, string, EnvironmentCapsuleState) error
}

type EnvironmentCreateActions interface {
	EnvironmentCreationActions
	EnvironmentCreationCapsuleActions
}

type EnvironmentCreationRepository interface {
	RecordEnvironmentCreateInvocation(context.Context, string, string, time.Time) (domain.EnvironmentCreation, error)
	InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (domain.EnvironmentState, error)
	ReserveInitialRuntime(context.Context, string, domain.RuntimeReservation) (domain.Runtime, error)
	PersistEnvironmentCreateRuntimeTransition(context.Context, string, int64, domain.RuntimeSnapshot) error
	FinishEnvironmentCreateOperation(context.Context, string, domain.OperationStatus, string, string, time.Time) error
	CompleteEnvironmentCreation(context.Context, string, time.Time) (domain.EnvironmentCreation, error)
}

type environmentCreationActions struct {
	repository EnvironmentCreationRepository
}

func NewEnvironmentCreationActions(repository EnvironmentCreationRepository, resolver PinnedProfileVersionResolver) (EnvironmentCreateActions, error) {
	if repository == nil {
		return nil, errors.New("new Environment creation actions: repository is required")
	}
	if resolver == nil {
		return nil, errors.New("new Environment creation actions: pinned Profile Version resolver is required")
	}
	capsuleRepository, ok := any(repository).(EnvironmentCapsuleStateRepository)
	if !ok {
		return nil, errors.New("new Environment creation actions: Capsule state repository is required")
	}
	actions := &environmentCreationActions{repository: repository}
	capsuleActions := NewEnvironmentCreationCapsuleActions(resolver, capsuleRepository)
	return &environmentCreationActionsWithCapsules{EnvironmentCreationActions: actions, capsuleActions: capsuleActions}, nil
}

type environmentCreationActionsWithCapsules struct {
	EnvironmentCreationActions
	capsuleActions EnvironmentCreationCapsuleActions
}

func (actions *environmentCreationActionsWithCapsules) ResolvePinnedProfileVersion(ctx context.Context, operationID string, at time.Time) (EnvironmentCapsuleState, error) {
	return actions.capsuleActions.ResolvePinnedProfileVersion(ctx, operationID, at)
}

func (actions *environmentCreationActionsWithCapsules) PersistEnvironmentCapsuleState(ctx context.Context, operationID string, state EnvironmentCapsuleState) error {
	return actions.capsuleActions.PersistEnvironmentCapsuleState(ctx, operationID, state)
}

func (actions *environmentCreationActions) RecordEnvironmentCreateInvocation(ctx context.Context, operationID, invocationID string, at time.Time) (EnvironmentCreateInvocation, error) {
	creation, err := actions.repository.RecordEnvironmentCreateInvocation(ctx, operationID, invocationID, at)
	if err != nil {
		return EnvironmentCreateInvocation{}, err
	}
	environment := creation.Environment().Snapshot()
	return EnvironmentCreateInvocation{
		OwnerUserID: environment.OwnerUserID, EnvironmentID: environment.ID, ProjectSeedID: creation.ProjectSeedID(),
	}, nil
}

func (actions *environmentCreationActions) InventoryEnvironmentState(ctx context.Context, operationID string, reservation domain.EnvironmentStateReservation) (string, error) {
	state, err := actions.repository.InventoryEnvironmentState(ctx, operationID, reservation)
	if err != nil {
		return "", err
	}
	return state.DataVolumeProviderID(), nil
}

func (actions *environmentCreationActions) ReserveInitialRuntime(ctx context.Context, operationID string, reservation domain.RuntimeReservation) (string, error) {
	runtime, err := actions.repository.ReserveInitialRuntime(ctx, operationID, reservation)
	if err != nil {
		return "", err
	}
	return runtime.Snapshot().ID, nil
}

func (actions *environmentCreationActions) PersistEnvironmentCreateRuntimeTransition(ctx context.Context, operationID string, expectedVersion int64, next domain.RuntimeSnapshot) error {
	return actions.repository.PersistEnvironmentCreateRuntimeTransition(ctx, operationID, expectedVersion, next)
}

func (actions *environmentCreationActions) RecordEnvironmentCreateOutcome(ctx context.Context, operationID string, outcome EnvironmentCreateOperationOutcome, at time.Time) error {
	status := domain.OperationFailed
	if outcome.Kind == EnvironmentCreateOutcomeRequiresInput {
		status = domain.OperationBlocked
	}
	return actions.repository.FinishEnvironmentCreateOperation(ctx, operationID, status, outcome.Code, outcome.Message, at)
}

func (actions *environmentCreationActions) CompleteEnvironmentCreation(ctx context.Context, operationID string, at time.Time) error {
	_, err := actions.repository.CompleteEnvironmentCreation(ctx, operationID, at)
	return err
}

type IDGenerator interface {
	NewID() string
}

type EnvironmentCreateOutput struct {
	DataVolumeProviderID string `json:"dataVolumeProviderId"`
	RuntimeID            string `json:"runtimeId"`
}

type EnvironmentCreateProvider interface {
	provider.DataVolumeProvider
	provider.RuntimeProvider
}

type EnvironmentCreateGuestRequest struct {
	OperationID string `json:"operationId"`
	RuntimeGuestReadinessRequest
}

type EnvironmentSSHIdentityRestorer interface {
	// RestoreEnvironmentSSHIdentity restores only the Environment-owned SSH
	// host identity. Active user public keys are reconciled through the shared
	// RuntimeSSHKeyReconciler seam after guest readiness.
	RestoreEnvironmentSSHIdentity(context.Context, EnvironmentCreateGuestRequest) error
}

type EnvironmentProjectSeedRequest struct {
	Guest         EnvironmentCreateGuestRequest `json:"guest"`
	ProjectSeedID string                        `json:"projectSeedId"`
}

type EnvironmentProjectSeedApplicator interface {
	// EnsureEnvironmentProjectSeedApplied must be idempotent for the stable
	// Operation and Project Seed identities in request. A retry must never
	// overwrite user-owned content produced by the first attempt.
	EnsureEnvironmentProjectSeedApplied(context.Context, EnvironmentProjectSeedRequest) error
}

type EnvironmentCapsuleMaterializationRequest struct {
	Guest EnvironmentCreateGuestRequest `json:"guest"`
	State EnvironmentCapsuleState       `json:"state"`
}

type EnvironmentCapsuleMaterializer interface {
	// EnsureEnvironmentCapsuleMaterialized must replay the same converged result
	// for a stable Operation ID and Capsule Lock digest after an ambiguous
	// transport failure.
	EnsureEnvironmentCapsuleMaterialized(context.Context, EnvironmentCapsuleMaterializationRequest) ([]profile.ProfileMaterializationResult, error)
}

type EnvironmentCredentialBindingStatus string

const (
	EnvironmentCredentialsBound        EnvironmentCredentialBindingStatus = "bound"
	EnvironmentCredentialsNotRequired  EnvironmentCredentialBindingStatus = "not_required"
	EnvironmentCredentialsRequireInput EnvironmentCredentialBindingStatus = "requires_input"
)

type EnvironmentCredentialBindingOutcome struct {
	Status  EnvironmentCredentialBindingStatus `json:"status"`
	Code    string                             `json:"code,omitempty"`
	Message string                             `json:"message,omitempty"`
}

type EnvironmentCredentialBinder interface {
	BindEnvironmentCredentials(context.Context, EnvironmentCreateGuestRequest) (EnvironmentCredentialBindingOutcome, error)
}

// NoProjectCredentialBinder is the honest S4 binding while the Project
// aggregate and Credential Requirements remain S9 scope.
type NoProjectCredentialBinder struct{}

func (NoProjectCredentialBinder) BindEnvironmentCredentials(context.Context, EnvironmentCreateGuestRequest) (EnvironmentCredentialBindingOutcome, error) {
	return EnvironmentCredentialBindingOutcome{Status: EnvironmentCredentialsNotRequired}, nil
}

type EnvironmentToolchainValidator interface {
	ValidateEnvironmentToolchain(context.Context, EnvironmentCreateGuestRequest) error
}

type EnvironmentCreateDependencies struct {
	Provider             EnvironmentCreateProvider
	Actions              EnvironmentCreationActions
	Capsules             EnvironmentCreationCapsuleActions
	SSHIdentity          EnvironmentSSHIdentityRestorer
	GuestReadiness       RuntimeGuestReadinessSource
	SSHKeys              RuntimeSSHKeyReconciler
	ProjectSeed          EnvironmentProjectSeedApplicator
	Materializer         EnvironmentCapsuleMaterializer
	Credentials          EnvironmentCredentialBinder
	Toolchain            EnvironmentToolchainValidator
	IDs                  IDGenerator
	Now                  func() time.Time
	ImageVersion         string
	ProviderPollInterval time.Duration
	ProviderPollTimeout  time.Duration
	GuestPollInterval    time.Duration
	GuestPollTimeout     time.Duration
}

type environmentCreateWorkflow struct{ dependencies EnvironmentCreateDependencies }

// EnvironmentCreateDefinition preserves the original construction surface
// while adapting it to the complete workflow. New bindings should use
// EnvironmentCreateDefinitionWithDependencies to provide real guest seams.
func EnvironmentCreateDefinition(dataVolumes provider.DataVolumeProvider, actions EnvironmentCreationActions, ids IDGenerator, now func() time.Time, imageVersion string) restate.ServiceDefinition {
	providerAdapter, _ := dataVolumes.(EnvironmentCreateProvider)
	allActions, _ := actions.(EnvironmentCreateActions)
	return EnvironmentCreateDefinitionWithDependencies(legacyEnvironmentCreateDependencies(providerAdapter, allActions, ids, now, imageVersion))
}

func EnvironmentCreateDefinitionWithDependencies(dependencies EnvironmentCreateDependencies) restate.ServiceDefinition {
	workflow := &environmentCreateWorkflow{dependencies: dependencies}
	return restate.NewWorkflow(EnvironmentCreateService).Handler(
		RunHandler,
		restate.NewWorkflowHandler(workflow.Run),
	)
}

func legacyEnvironmentCreateDependencies(providerAdapter EnvironmentCreateProvider, actions EnvironmentCreateActions, ids IDGenerator, now func() time.Time, imageVersion string) EnvironmentCreateDependencies {
	guest := legacyEnvironmentCreateGuest{}
	return EnvironmentCreateDependencies{
		Provider: providerAdapter, Actions: actions, Capsules: actions,
		SSHIdentity: guest, GuestReadiness: guest, SSHKeys: guest, ProjectSeed: guest, Materializer: guest,
		Credentials: NoProjectCredentialBinder{}, Toolchain: guest,
		IDs: ids, Now: now, ImageVersion: imageVersion,
	}
}

type legacyEnvironmentCreateGuest struct{}

func (legacyEnvironmentCreateGuest) RestoreEnvironmentSSHIdentity(context.Context, EnvironmentCreateGuestRequest) error {
	return nil
}

func (legacyEnvironmentCreateGuest) WaitForRuntimeReady(_ context.Context, request RuntimeGuestReadinessRequest) (RuntimeGuestReadiness, error) {
	return RuntimeGuestReadiness{BootID: "foundation-boot", PrivateIPv4: request.PrivateIPv4, DataMounted: true}, nil
}

func (legacyEnvironmentCreateGuest) ReconcileRuntimeSSHKeys(context.Context, RuntimeGuestReadinessRequest) error {
	return nil
}

func (legacyEnvironmentCreateGuest) EnsureEnvironmentProjectSeedApplied(context.Context, EnvironmentProjectSeedRequest) error {
	return nil
}

func (legacyEnvironmentCreateGuest) EnsureEnvironmentCapsuleMaterialized(_ context.Context, request EnvironmentCapsuleMaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	return request.State.ApplyResults, nil
}

func (legacyEnvironmentCreateGuest) ValidateEnvironmentToolchain(context.Context, EnvironmentCreateGuestRequest) error {
	return nil
}

func (workflow *environmentCreateWorkflow) Run(ctx restate.WorkflowContext, input domain.EnvironmentCreateDispatch) (EnvironmentCreateOutput, error) {
	if restate.Key(ctx) != input.OperationID {
		return EnvironmentCreateOutput{}, restate.TerminalErrorf("workflow key does not match Operation ID")
	}
	dependencies := workflow.dependencies
	if dependencies.Actions == nil || dependencies.Now == nil {
		return EnvironmentCreateOutput{}, restate.TerminalErrorf("Environment create workflow projection dependencies are incomplete")
	}
	invocation, recordErr := restate.Run(ctx, func(runCtx restate.RunContext) (EnvironmentCreateInvocation, error) {
		invocation, recordErr := dependencies.Actions.RecordEnvironmentCreateInvocation(runCtx, input.OperationID, runCtx.Request().ID, dependencies.Now())
		return invocation, classifyDurableError(recordErr)
	}, restate.WithName("record-invocation"))
	if recordErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: recordErr.Error(),
		}, dependencies.Now)
	}
	if err := validateEnvironmentCreateInvocation(input, invocation); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error(),
		}, dependencies.Now)
	}
	if err := validateEnvironmentCreateDependencies(dependencies); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error(),
		}, dependencies.Now)
	}
	volume, durableErr := restate.Run(ctx, func(runCtx restate.RunContext) (provider.DataVolume, error) {
		volume, err := dependencies.Provider.EnsureDataVolume(runCtx, provider.EnsureDataVolumeRequest{
			EnvironmentID: input.EnvironmentID, OperationID: input.OperationID,
			Region: input.Region, AvailabilityZone: input.AvailabilityZone,
		})
		return volume, classifyDurableError(err)
	}, restate.WithName("ensure-data-volume"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: durableErr.Error(),
		}, dependencies.Now)
	}
	if err := validateDataVolume(input, volume); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: string(provider.ErrorCodeResourceDiverged), Message: err.Error(),
		}, dependencies.Now)
	}
	seed, durableErr := restate.Run(ctx, func(restate.RunContext) (environmentStateReservationSeed, error) {
		return environmentStateReservationSeed{
			BackendResourceID: dependencies.IDs.NewID(), WorkspaceID: dependencies.IDs.NewID(), HomeID: dependencies.IDs.NewID(),
			ServicesID: dependencies.IDs.NewID(), CacheID: dependencies.IDs.NewID(), RuntimeID: dependencies.IDs.NewID(),
			ImageVersion: dependencies.ImageVersion, CreatedAt: dependencies.Now(),
		}, nil
	}, restate.WithName("reserve-creation-identities"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: durableErr.Error(),
		}, dependencies.Now)
	}
	inventory, durableErr := restate.Run(ctx, func(runCtx restate.RunContext) (environmentStateInventory, error) {
		metadata, err := json.Marshal(struct {
			AvailabilityZone string `json:"availabilityZone"`
		}{AvailabilityZone: volume.AvailabilityZone})
		if err != nil {
			return environmentStateInventory{}, classifyDurableError(err)
		}
		providerID, err := dependencies.Actions.InventoryEnvironmentState(runCtx, input.OperationID, domain.EnvironmentStateReservation{
			BackendResourceID: seed.BackendResourceID, WorkspaceID: seed.WorkspaceID, HomeID: seed.HomeID,
			ServicesID: seed.ServicesID, CacheID: seed.CacheID, Provider: volume.Provider,
			ProviderID: volume.ProviderID, Metadata: metadata, CreatedAt: seed.CreatedAt,
		})
		if err != nil {
			return environmentStateInventory{}, classifyDurableError(err)
		}
		return environmentStateInventory{DataVolumeProviderID: providerID}, nil
	}, restate.WithName("inventory-data-volume"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: durableErr.Error(),
		}, dependencies.Now)
	}
	runtimeReservation := domain.RuntimeReservation{
		ID: seed.RuntimeID, EnvironmentID: input.EnvironmentID, Sequence: 1,
		RuntimePreset: input.RuntimePreset, Region: input.Region, AvailabilityZone: input.AvailabilityZone,
		ImageVersion: seed.ImageVersion, CreatedAt: seed.CreatedAt,
	}
	runtimeID, durableErr := restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
		runtimeID, err := dependencies.Actions.ReserveInitialRuntime(runCtx, input.OperationID, runtimeReservation)
		return runtimeID, classifyDurableError(err)
	}, restate.WithName("reserve-initial-runtime"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: durableErr.Error(),
		}, dependencies.Now)
	}
	if runtimeID != seed.RuntimeID {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: string(provider.ErrorCodeResourceDiverged), Message: "reserved Runtime identity diverged",
		}, dependencies.Now)
	}
	reservedRuntime, err := domain.ReserveRuntime(runtimeReservation)
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error(),
		}, dependencies.Now)
	}
	runtimeState := reservedRuntime.Snapshot()
	runtimeSpec := provider.RuntimeSpec{
		RuntimeID: runtimeID, EnvironmentID: input.EnvironmentID, Sequence: 1,
		Region: input.Region, AvailabilityZone: input.AvailabilityZone, RuntimePreset: input.RuntimePreset,
		ImageVersion: seed.ImageVersion, DataVolumeProviderID: inventory.DataVolumeProviderID,
	}
	ensured, durableErr := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		observed, ensureErr := dependencies.Provider.EnsureRuntime(runCtx, provider.EnsureRuntimeRequest{
			RuntimeSpec: runtimeSpec, OperationID: input.OperationID,
		})
		outcome, outcomeErr := providerOutcome(observed, ensureErr)
		return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
	}, restate.WithName("ensure-runtime-provider"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: durableErr.Error(),
		}, dependencies.Now)
	}
	if ensured.Failure != "" {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: ensured.FailureCode, Message: ensured.Failure,
		}, dependencies.Now)
	}
	lifecycleRequest := provider.RuntimeLifecycleRequest{RuntimeSpec: runtimeSpec, ProviderID: ensured.Runtime.ProviderID}
	if err := validateProviderRuntimeIdentity(ensured.Runtime, lifecycleRequest); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: string(provider.ErrorCodeResourceDiverged), Message: err.Error(),
		}, dependencies.Now)
	}
	provisioning, err := reservedRuntime.Provision(ensured.Runtime.ProviderID, ensured.ObservedAt)
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	runtimeState, err = persistEnvironmentCreateRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "persist-runtime-provisioning", runtimeState, provisioning)
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	if ensured.Runtime.State != provider.RuntimeStatePending && ensured.Runtime.State != provider.RuntimeStateRunning {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{
			Kind: EnvironmentCreateOutcomeFailed, Code: string(provider.ErrorCodeResourceDiverged), Message: fmt.Sprintf("provisioned Runtime state is %q", ensured.Runtime.State),
		}, dependencies.Now)
	}
	attached, durableErr := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		observed, attachErr := dependencies.Provider.EnsureRuntimeDataVolumeAttachment(runCtx, lifecycleRequest)
		outcome, outcomeErr := providerOutcome(observed, attachErr)
		return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
	}, restate.WithName("ensure-runtime-data-volume-attachment"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: durableErr.Error()}, dependencies.Now)
	}
	if attached.Failure != "" {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: attached.FailureCode, Message: attached.Failure}, dependencies.Now)
	}
	if err := validateProviderRuntimeIdentity(attached.Runtime, lifecycleRequest); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: string(provider.ErrorCodeResourceDiverged), Message: err.Error()}, dependencies.Now)
	}
	starting, err := provisioning.BeginStart(dependencies.Now())
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	runtimeState, err = persistEnvironmentCreateRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "persist-runtime-starting", runtimeState, starting)
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	running, err := waitForProviderState(ctx, dependencies.Provider, lifecycleRequest, ensured, provider.RuntimeStateRunning, provider.RuntimeStatePending, "wait-created-runtime-running", dependencies.ProviderPollInterval, dependencies.ProviderPollTimeout, dependencies.Now)
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	if running.Failure != "" {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: running.FailureCode, Message: running.Failure}, dependencies.Now)
	}
	if err := validateCreatedRuntimeNetworking(input, runtimeSpec, running.Runtime); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: string(provider.ErrorCodeResourceDiverged), Message: err.Error()}, dependencies.Now)
	}
	guestRequest := EnvironmentCreateGuestRequest{
		OperationID: input.OperationID,
		RuntimeGuestReadinessRequest: RuntimeGuestReadinessRequest{
			OwnerUserID: invocation.OwnerUserID, EnvironmentID: input.EnvironmentID, RuntimeID: runtimeID,
			ProviderID: running.Runtime.ProviderID, PrivateIPv4: running.Runtime.PrivateIPv4,
		},
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.SSHIdentity.RestoreEnvironmentSSHIdentity(runCtx, guestRequest))
	}, restate.WithName("restore-environment-ssh-identity")); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	ready, err := waitForEnvironmentGuestReadiness(ctx, dependencies.GuestReadiness, guestRequest.RuntimeGuestReadinessRequest, dependencies.GuestPollInterval, dependencies.GuestPollTimeout, dependencies.Now)
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: GuestNotReady, Message: err.Error()}, dependencies.Now)
	}
	guestRequest.PrivateIPv4 = ready.PrivateIPv4
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.SSHKeys.ReconcileRuntimeSSHKeys(runCtx, guestRequest.RuntimeGuestReadinessRequest))
	}, restate.WithName("reconcile-environment-ssh-keys")); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.ProjectSeed.EnsureEnvironmentProjectSeedApplied(runCtx, EnvironmentProjectSeedRequest{Guest: guestRequest, ProjectSeedID: invocation.ProjectSeedID}))
	}, restate.WithName("apply-project-seed")); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: "PROJECT_SEED_INVALID", Message: err.Error()}, dependencies.Now)
	}
	state, durableErr := restate.Run(ctx, func(runCtx restate.RunContext) (EnvironmentCapsuleState, error) {
		state, resolveErr := dependencies.Capsules.ResolvePinnedProfileVersion(runCtx, input.OperationID, dependencies.Now().UTC())
		return state, classifyDurableError(resolveErr)
	}, restate.WithName("resolve-profile-version"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: durableErr.Error()}, dependencies.Now)
	}
	if state.UpgradePolicy == "" {
		state.UpgradePolicy = domain.UpgradeManual
	}
	if err := validateEnvironmentCapsuleState(input, state); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: "PROFILE_INCOMPATIBLE", Message: err.Error()}, dependencies.Now)
	}
	applyResults, durableErr := restate.Run(ctx, func(runCtx restate.RunContext) ([]profile.ProfileMaterializationResult, error) {
		results, materializeErr := dependencies.Materializer.EnsureEnvironmentCapsuleMaterialized(runCtx, EnvironmentCapsuleMaterializationRequest{Guest: guestRequest, State: state})
		return results, classifyDurableError(materializeErr)
	}, restate.WithName("materialize-capsule-lock"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: "PROFILE_CONFLICT", Message: durableErr.Error()}, dependencies.Now)
	}
	if err := validateEnvironmentMaterializationResults(state.CapsuleLock, applyResults); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: "PROFILE_CONFLICT", Message: err.Error()}, dependencies.Now)
	}
	state.ApplyResults = applyResults
	state.Materializations = InstalledMaterializationsFromApplyResults(applyResults)
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Capsules.PersistEnvironmentCapsuleState(runCtx, input.OperationID, state))
	}, restate.WithName("persist-capsule-state")); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	binding, durableErr := restate.Run(ctx, func(runCtx restate.RunContext) (EnvironmentCredentialBindingOutcome, error) {
		outcome, bindErr := dependencies.Credentials.BindEnvironmentCredentials(runCtx, guestRequest)
		return outcome, classifyDurableError(bindErr)
	}, restate.WithName("bind-environment-credentials"))
	if durableErr != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: durableErr.Error()}, dependencies.Now)
	}
	if err := validateCredentialBindingOutcome(binding); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	if binding.Status == EnvironmentCredentialsRequireInput {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, nil, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeRequiresInput, Code: binding.Code, Message: binding.Message}, dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Toolchain.ValidateEnvironmentToolchain(runCtx, guestRequest))
	}, restate.WithName("validate-environment-toolchain")); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: "PROFILE_INCOMPATIBLE", Message: err.Error()}, dependencies.Now)
	}
	runtime, err := domain.RestoreRuntime(runtimeState)
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	markedReady, err := runtime.MarkReady(domain.RuntimeReadinessObservation{
		ProviderInstanceRef: running.Runtime.ProviderID, BootID: ready.BootID, PrivateAddress: ready.PrivateIPv4,
		ExpectedVersion: runtimeState.Version, ObservedAt: dependencies.Now(),
	})
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	runtimeState, err = persistEnvironmentCreateRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "persist-runtime-ready", runtimeState, markedReady)
	if err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.CompleteEnvironmentCreation(runCtx, input.OperationID, dependencies.Now()))
	}, restate.WithName("complete-projection")); err != nil {
		return EnvironmentCreateOutput{}, failEnvironmentCreation(ctx, dependencies.Actions, input.OperationID, &runtimeState, EnvironmentCreateOperationOutcome{Kind: EnvironmentCreateOutcomeFailed, Code: EnvironmentCreateFailed, Message: err.Error()}, dependencies.Now)
	}
	return EnvironmentCreateOutput{DataVolumeProviderID: inventory.DataVolumeProviderID, RuntimeID: runtimeID}, nil
}

func validateEnvironmentCreateDependencies(dependencies EnvironmentCreateDependencies) error {
	if dependencies.Provider == nil || dependencies.Actions == nil || dependencies.Capsules == nil ||
		dependencies.SSHIdentity == nil || dependencies.GuestReadiness == nil || dependencies.SSHKeys == nil || dependencies.ProjectSeed == nil ||
		dependencies.Materializer == nil || dependencies.Credentials == nil || dependencies.Toolchain == nil ||
		dependencies.IDs == nil || dependencies.Now == nil || strings.TrimSpace(dependencies.ImageVersion) == "" {
		return errors.New("Environment create workflow dependencies are incomplete")
	}
	return nil
}

func validateEnvironmentCreateInvocation(input domain.EnvironmentCreateDispatch, invocation EnvironmentCreateInvocation) error {
	if strings.TrimSpace(invocation.OwnerUserID) == "" || strings.TrimSpace(invocation.ProjectSeedID) == "" ||
		invocation.EnvironmentID != input.EnvironmentID {
		return errors.New("Environment create invocation ownership or Project Seed diverged")
	}
	return nil
}

func persistEnvironmentCreateRuntimeTransition(ctx restate.WorkflowContext, actions EnvironmentCreationActions, operationID, step string, before domain.RuntimeSnapshot, after domain.Runtime) (domain.RuntimeSnapshot, error) {
	next := after.Snapshot()
	return restate.Run(ctx, func(runCtx restate.RunContext) (domain.RuntimeSnapshot, error) {
		if err := actions.PersistEnvironmentCreateRuntimeTransition(runCtx, operationID, before.Version, next); err != nil {
			return domain.RuntimeSnapshot{}, classifyDurableError(err)
		}
		return next, nil
	}, restate.WithName(step))
}

func failEnvironmentCreation(ctx restate.WorkflowContext, actions EnvironmentCreationActions, operationID string, current *domain.RuntimeSnapshot, outcome EnvironmentCreateOperationOutcome, now func() time.Time) error {
	if outcome.Kind == "" {
		outcome.Kind = EnvironmentCreateOutcomeFailed
	}
	if outcome.Code == "" {
		outcome.Code = EnvironmentCreateFailed
	}
	if outcome.Message == "" {
		outcome.Message = outcome.Code
	}
	var runtimeTransitionErr error
	if current != nil && outcome.Kind == EnvironmentCreateOutcomeFailed && current.Status != domain.RuntimeError {
		runtime, err := domain.RestoreRuntime(*current)
		if err != nil {
			runtimeTransitionErr = err
		} else {
			var failed domain.Runtime
			if current.Status == domain.RuntimeAbsent {
				failed, err = runtime.MarkProvisionError(now())
			} else if current.ProviderInstanceRef != nil {
				failed, err = runtime.MarkError(domain.RuntimeStateObservation{
					ProviderInstanceRef: *current.ProviderInstanceRef, ExpectedVersion: current.Version, ObservedAt: now(),
				})
			}
			runtimeTransitionErr = err
			if runtimeTransitionErr == nil && failed.Snapshot().ID != "" {
				_, runtimeTransitionErr = persistEnvironmentCreateRuntimeTransition(ctx, actions, operationID, "mark-environment-create-runtime-error", *current, failed)
			}
		}
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(actions.RecordEnvironmentCreateOutcome(runCtx, operationID, outcome, now()))
	}, restate.WithName("record-environment-create-"+strings.ReplaceAll(string(outcome.Kind), "_", "-"))); err != nil {
		return err
	}
	if runtimeTransitionErr != nil {
		return restate.TerminalErrorf("%s: %s; Runtime error transition failed: %s", outcome.Code, outcome.Message, runtimeTransitionErr)
	}
	return restate.TerminalErrorf("%s: %s", outcome.Code, outcome.Message)
}

func validateCreatedRuntimeNetworking(input domain.EnvironmentCreateDispatch, spec provider.RuntimeSpec, observed provider.Runtime) error {
	request := provider.RuntimeLifecycleRequest{RuntimeSpec: spec, ProviderID: observed.ProviderID}
	if err := validateProviderRuntimeIdentity(observed, request); err != nil {
		return err
	}
	if observed.State != provider.RuntimeStateRunning {
		return fmt.Errorf("Runtime networking observation state is %q, want %q", observed.State, provider.RuntimeStateRunning)
	}
	address, err := netip.ParseAddr(observed.PrivateIPv4)
	if err != nil || !address.Is4() || !address.IsPrivate() {
		return errors.New("Runtime networking requires a private IPv4 address")
	}
	if observed.Region != input.Region || observed.AvailabilityZone != input.AvailabilityZone {
		return errors.New("Runtime networking placement diverged")
	}
	return nil
}

type environmentGuestReadinessOutcome struct {
	Readiness        RuntimeGuestReadiness `json:"readiness"`
	RetryableFailure bool                  `json:"retryableFailure,omitempty"`
	Failure          string                `json:"failure,omitempty"`
}

func waitForEnvironmentGuestReadiness(ctx restate.WorkflowContext, source RuntimeGuestReadinessSource, request RuntimeGuestReadinessRequest, interval, timeout time.Duration, now func() time.Time) (RuntimeGuestReadiness, error) {
	if interval <= 0 {
		interval = defaultProviderPollInterval
	}
	if timeout <= 0 {
		timeout = defaultProviderPollTimeout
	}
	poll, err := durableDeadlinePoll(ctx, nil, durableDeadlinePollConfig{
		timeout: timeout, initialDelay: interval, maxDelay: 30 * time.Second,
		stepPrefix: "wait-created-runtime-guest-ready", readStepPrefix: "read-created-runtime-guest-readiness", now: now,
	}, func(runCtx restate.RunContext, _ time.Time) (durableDeadlinePollRead[environmentGuestReadinessOutcome], error) {
		readiness, readErr := source.WaitForRuntimeReady(runCtx, request)
		if readErr == nil {
			return durableDeadlinePollRead[environmentGuestReadinessOutcome]{Value: environmentGuestReadinessOutcome{Readiness: readiness}, UseValue: true}, nil
		}
		var classified interface{ Transient() bool }
		if errors.As(readErr, &classified) && classified.Transient() {
			return durableDeadlinePollRead[environmentGuestReadinessOutcome]{Value: environmentGuestReadinessOutcome{RetryableFailure: true}, UseValue: true, RetryableFailure: true}, nil
		}
		return durableDeadlinePollRead[environmentGuestReadinessOutcome]{Value: environmentGuestReadinessOutcome{Failure: readErr.Error()}, UseValue: true}, nil
	}, func(outcome environmentGuestReadinessOutcome, _ time.Time) (environmentGuestReadinessOutcome, bool) {
		if outcome.RetryableFailure {
			return outcome, false
		}
		if outcome.Failure != "" {
			return outcome, true
		}
		ready := outcome.Readiness
		return outcome, ready.DataMounted && ready.BootID != "" && ready.PrivateIPv4 == request.PrivateIPv4
	}, nil)
	if err != nil {
		return RuntimeGuestReadiness{}, err
	}
	if poll.timedOut {
		return RuntimeGuestReadiness{}, errors.New("guest did not report current boot and mounted data before the durable wait deadline")
	}
	if poll.value.Failure != "" {
		return RuntimeGuestReadiness{}, errors.New(poll.value.Failure)
	}
	return poll.value.Readiness, nil
}

func validateCredentialBindingOutcome(outcome EnvironmentCredentialBindingOutcome) error {
	switch outcome.Status {
	case EnvironmentCredentialsBound, EnvironmentCredentialsNotRequired:
		if outcome.Code != "" || outcome.Message != "" {
			return errors.New("completed credential binding cannot carry an error outcome")
		}
		return nil
	case EnvironmentCredentialsRequireInput:
		if outcome.Code != CredentialRequired || outcome.Message == "" {
			return errors.New("requires_input credential binding requires CREDENTIAL_REQUIRED and a message")
		}
		return nil
	default:
		return fmt.Errorf("unknown credential binding status %q", outcome.Status)
	}
}

func validateEnvironmentCapsuleState(input domain.EnvironmentCreateDispatch, state EnvironmentCapsuleState) error {
	if state.CapsuleLock.ID == "" || state.CapsuleLock.EnvironmentID != input.EnvironmentID || state.CapsuleLock.ProfileVersionID == "" {
		return errors.New("Environment Capsule state has an invalid Capsule Lock")
	}
	if !state.UpgradePolicy.Valid() {
		return fmt.Errorf("Environment Capsule state has invalid upgrade policy %q", state.UpgradePolicy)
	}
	return nil
}

func validateEnvironmentMaterializationResults(lock domain.CapsuleLockSnapshot, results []profile.ProfileMaterializationResult) error {
	if len(results) != len(lock.ResolvedComponents) {
		return fmt.Errorf("Capsule Lock materialization returned %d results for %d resolved Components", len(results), len(lock.ResolvedComponents))
	}
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		component, ok := lock.ResolvedComponents[result.ComponentID]
		if !ok {
			return fmt.Errorf("Capsule Lock materialization returned unknown Component %q", result.ComponentID)
		}
		if _, duplicate := seen[result.ComponentID]; duplicate {
			return fmt.Errorf("Capsule Lock materialization returned duplicate Component %q", result.ComponentID)
		}
		seen[result.ComponentID] = struct{}{}
		if result.ID == "" || result.LockID != lock.ID || result.LockDigest != lock.Digest ||
			result.CapsuleDigest != component.CapsuleDigest || result.ComponentDigest != component.ComponentDigest ||
			result.Scope != component.Scope {
			return fmt.Errorf("Capsule Lock materialization identity diverged for Component %q", result.ComponentID)
		}
		switch result.Operation {
		case profile.OperationCreate, profile.OperationUpdate, profile.OperationSkip:
		default:
			return fmt.Errorf("Capsule Lock materialization did not converge for Component %q: operation %q", result.ComponentID, result.Operation)
		}
		if result.ApprovalRequired {
			return fmt.Errorf("Capsule Lock materialization still requires approval for Component %q", result.ComponentID)
		}
		switch result.Mode {
		case profile.MaterializationManaged, profile.MaterializationSeeded:
		case profile.MaterializationReferenced:
			if result.Operation != profile.OperationSkip || result.RequirementState != profile.RequirementBound {
				return fmt.Errorf("referenced Capsule Lock materialization did not bind Component %q", result.ComponentID)
			}
			continue
		default:
			return fmt.Errorf("Capsule Lock materialization returned invalid mode %q for Component %q", result.Mode, result.ComponentID)
		}
		if result.DesiredDigest == "" || result.LastAppliedDigest != result.DesiredDigest || result.ObservedDigest != result.DesiredDigest {
			return fmt.Errorf("Capsule Lock materialization digests did not converge for Component %q", result.ComponentID)
		}
	}
	return nil
}

func InstalledMaterializationsFromApplyResults(results []profile.ProfileMaterializationResult) []profile.InstalledMaterialization {
	return profile.InstalledMaterializationsFromResults(results)
}

func validateDataVolume(input domain.EnvironmentCreateDispatch, volume provider.DataVolume) error {
	if volume.EnvironmentID != input.EnvironmentID || volume.Region != input.Region || volume.AvailabilityZone != input.AvailabilityZone ||
		volume.Provider == "" || volume.Provider != strings.TrimSpace(volume.Provider) ||
		volume.ProviderID == "" || volume.ProviderID != strings.TrimSpace(volume.ProviderID) {
		return restate.TerminalErrorf("Data Volume ownership or placement does not match Environment creation")
	}
	return nil
}

func classifyDurableError(err error) error {
	if errors.Is(err, errInvalidEnvironmentCapsuleState) ||
		errors.Is(err, dbstore.ErrEnvironmentMaterializationLockMismatch) ||
		errors.Is(err, dbstore.ErrCapsuleLockConflict) {
		return restate.ToTerminalError(err)
	}
	var classified interface{ Transient() bool }
	if errors.As(err, &classified) && !classified.Transient() {
		return restate.ToTerminalError(err)
	}
	return err
}

type environmentStateInventory struct {
	DataVolumeProviderID string `json:"dataVolumeProviderId"`
}

type environmentStateReservationSeed struct {
	BackendResourceID string    `json:"backendResourceId"`
	WorkspaceID       string    `json:"workspaceId"`
	HomeID            string    `json:"homeId"`
	ServicesID        string    `json:"servicesId"`
	CacheID           string    `json:"cacheId"`
	RuntimeID         string    `json:"runtimeId"`
	ImageVersion      string    `json:"imageVersion"`
	CreatedAt         time.Time `json:"createdAt"`
}

type Client struct {
	ingress *ingress.Client
}

func NewClient(client *ingress.Client) *Client {
	return &Client{ingress: client}
}

func (client *Client) SendEnvironmentCreate(ctx context.Context, input domain.EnvironmentCreateDispatch) error {
	_, err := ingress.WorkflowSend[domain.EnvironmentCreateDispatch](
		client.ingress, EnvironmentCreateService, input.OperationID, RunHandler,
	).Send(ctx, input)
	return err
}
