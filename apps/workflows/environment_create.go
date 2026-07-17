package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

const (
	EnvironmentCreateService = "EnvironmentCreate"
	RunHandler               = "Run"
)

type EnvironmentCreationActions interface {
	RecordEnvironmentCreateInvocation(context.Context, string, string, time.Time) error
	InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (string, error)
	ReserveInitialRuntime(context.Context, string, domain.RuntimeReservation) (string, error)
	CompleteEnvironmentCreation(context.Context, string, time.Time) error
}

// EnvironmentCapsuleState is the result of the existing profile.resolve
// pathway and the guest apply-results seam. The workflow persists it only
// after runtime validation has completed.
type EnvironmentCapsuleState struct {
	CapsuleLock      domain.CapsuleLockSnapshot           `json:"capsuleLock"`
	UpgradePolicy    domain.UpgradePolicy                 `json:"upgradePolicy"`
	ApplyResults     []guest.ProfileMaterializationResult `json:"applyResults,omitempty"`
	Materializations []guest.InstalledMaterialization     `json:"materializations,omitempty"`
}

type EnvironmentCreationCapsuleActions interface {
	ResolvePinnedProfileVersion(context.Context, string, time.Time) (EnvironmentCapsuleState, error)
	PersistEnvironmentCapsuleState(context.Context, string, EnvironmentCapsuleState) error
}

type EnvironmentCreationRepository interface {
	RecordEnvironmentCreateInvocation(context.Context, string, string, time.Time) (domain.EnvironmentCreation, error)
	InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (domain.EnvironmentState, error)
	ReserveInitialRuntime(context.Context, string, domain.RuntimeReservation) (domain.Runtime, error)
	CompleteEnvironmentCreation(context.Context, string, time.Time) (domain.EnvironmentCreation, error)
}

type environmentCreationActions struct {
	repository EnvironmentCreationRepository
}

func NewEnvironmentCreationActions(repository EnvironmentCreationRepository, resolver PinnedProfileVersionResolver) (EnvironmentCreationActions, error) {
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

func (actions *environmentCreationActions) RecordEnvironmentCreateInvocation(ctx context.Context, operationID, invocationID string, at time.Time) error {
	_, err := actions.repository.RecordEnvironmentCreateInvocation(ctx, operationID, invocationID, at)
	return err
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

type environmentCreateWorkflow struct {
	provider provider.DataVolumeProvider
	actions  EnvironmentCreationActions
	ids      IDGenerator
	now      func() time.Time
	image    string
}

func EnvironmentCreateDefinition(dataVolumes provider.DataVolumeProvider, actions EnvironmentCreationActions, ids IDGenerator, now func() time.Time, imageVersion string) restate.ServiceDefinition {
	workflow := &environmentCreateWorkflow{provider: dataVolumes, actions: actions, ids: ids, now: now, image: imageVersion}
	return restate.NewWorkflow(EnvironmentCreateService).Handler(
		RunHandler,
		restate.NewWorkflowHandler(workflow.Run),
	)
}

func (workflow *environmentCreateWorkflow) Run(ctx restate.WorkflowContext, input domain.EnvironmentCreateDispatch) (EnvironmentCreateOutput, error) {
	if restate.Key(ctx) != input.OperationID {
		return EnvironmentCreateOutput{}, restate.TerminalErrorf("workflow key does not match Operation ID")
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(workflow.actions.RecordEnvironmentCreateInvocation(runCtx, input.OperationID, runCtx.Request().ID, workflow.now()))
	}, restate.WithName("record-invocation")); err != nil {
		return EnvironmentCreateOutput{}, err
	}
	volume, err := restate.Run(ctx, func(runCtx restate.RunContext) (provider.DataVolume, error) {
		volume, err := workflow.provider.EnsureDataVolume(runCtx, provider.EnsureDataVolumeRequest{
			EnvironmentID: input.EnvironmentID, OperationID: input.OperationID,
			Region: input.Region, AvailabilityZone: input.AvailabilityZone,
		})
		return volume, classifyDurableError(err)
	}, restate.WithName("ensure-data-volume"))
	if err != nil {
		return EnvironmentCreateOutput{}, err
	}
	if err := validateDataVolume(input, volume); err != nil {
		return EnvironmentCreateOutput{}, err
	}
	seed, err := restate.Run(ctx, func(restate.RunContext) (environmentStateReservationSeed, error) {
		return environmentStateReservationSeed{
			BackendResourceID: workflow.ids.NewID(), WorkspaceID: workflow.ids.NewID(), HomeID: workflow.ids.NewID(),
			ServicesID: workflow.ids.NewID(), CacheID: workflow.ids.NewID(), RuntimeID: workflow.ids.NewID(),
			ImageVersion: workflow.image, CreatedAt: workflow.now(),
		}, nil
	}, restate.WithName("reserve-creation-identities"))
	if err != nil {
		return EnvironmentCreateOutput{}, err
	}
	inventory, err := restate.Run(ctx, func(runCtx restate.RunContext) (environmentStateInventory, error) {
		metadata, err := json.Marshal(struct {
			AvailabilityZone string `json:"availabilityZone"`
		}{AvailabilityZone: volume.AvailabilityZone})
		if err != nil {
			return environmentStateInventory{}, classifyDurableError(err)
		}
		providerID, err := workflow.actions.InventoryEnvironmentState(runCtx, input.OperationID, domain.EnvironmentStateReservation{
			BackendResourceID: seed.BackendResourceID, WorkspaceID: seed.WorkspaceID, HomeID: seed.HomeID,
			ServicesID: seed.ServicesID, CacheID: seed.CacheID, Provider: volume.Provider,
			ProviderID: volume.ProviderID, Metadata: metadata, CreatedAt: seed.CreatedAt,
		})
		if err != nil {
			return environmentStateInventory{}, classifyDurableError(err)
		}
		return environmentStateInventory{DataVolumeProviderID: providerID}, nil
	}, restate.WithName("inventory-data-volume"))
	if err != nil {
		return EnvironmentCreateOutput{}, err
	}
	runtimeID, err := restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
		runtimeID, err := workflow.actions.ReserveInitialRuntime(runCtx, input.OperationID, domain.RuntimeReservation{
			ID: seed.RuntimeID, EnvironmentID: input.EnvironmentID, Sequence: 1,
			RuntimePreset: input.RuntimePreset, Region: input.Region, AvailabilityZone: input.AvailabilityZone,
			ImageVersion: seed.ImageVersion, CreatedAt: seed.CreatedAt,
		})
		return runtimeID, classifyDurableError(err)
	}, restate.WithName("reserve-initial-runtime"))
	if err != nil {
		return EnvironmentCreateOutput{}, err
	}
	if capsuleActions, ok := workflow.actions.(EnvironmentCreationCapsuleActions); ok {
		state, err := restate.Run(ctx, func(runCtx restate.RunContext) (EnvironmentCapsuleState, error) {
			state, err := capsuleActions.ResolvePinnedProfileVersion(runCtx, input.OperationID, workflow.now().UTC())
			return state, classifyDurableError(err)
		}, restate.WithName("resolve-profile-version"))
		if err != nil {
			return EnvironmentCreateOutput{}, classifyDurableError(err)
		}
		if state.UpgradePolicy == "" {
			state.UpgradePolicy = domain.UpgradeManual
		}
		if err := validateEnvironmentCapsuleState(input, state); err != nil {
			return EnvironmentCreateOutput{}, restate.ToTerminalError(err)
		}
		state.Materializations = InstalledMaterializationsFromApplyResults(state.ApplyResults)
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
			return classifyDurableError(capsuleActions.PersistEnvironmentCapsuleState(runCtx, input.OperationID, state))
		}, restate.WithName("persist-capsule-state")); err != nil {
			return EnvironmentCreateOutput{}, classifyDurableError(err)
		}
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(workflow.actions.CompleteEnvironmentCreation(runCtx, input.OperationID, workflow.now()))
	}, restate.WithName("complete-projection")); err != nil {
		return EnvironmentCreateOutput{}, err
	}
	return EnvironmentCreateOutput{DataVolumeProviderID: inventory.DataVolumeProviderID, RuntimeID: runtimeID}, nil
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

func InstalledMaterializationsFromApplyResults(results []guest.ProfileMaterializationResult) []guest.InstalledMaterialization {
	return guest.InstalledMaterializationsFromResults(results)
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
