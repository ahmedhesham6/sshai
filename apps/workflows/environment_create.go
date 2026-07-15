package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

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
	CompleteEnvironmentCreation(context.Context, string, time.Time) error
}

type EnvironmentCreationRepository interface {
	RecordEnvironmentCreateInvocation(context.Context, string, string, time.Time) (domain.EnvironmentCreation, error)
	InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (domain.EnvironmentState, error)
	CompleteEnvironmentCreation(context.Context, string, time.Time) (domain.EnvironmentCreation, error)
}

type environmentCreationActions struct {
	repository EnvironmentCreationRepository
}

func NewEnvironmentCreationActions(repository EnvironmentCreationRepository) EnvironmentCreationActions {
	return &environmentCreationActions{repository: repository}
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

func (actions *environmentCreationActions) CompleteEnvironmentCreation(ctx context.Context, operationID string, at time.Time) error {
	_, err := actions.repository.CompleteEnvironmentCreation(ctx, operationID, at)
	return err
}

type IDGenerator interface {
	NewID() string
}

type EnvironmentCreateOutput struct {
	DataVolumeProviderID string `json:"dataVolumeProviderId"`
}

type environmentCreateWorkflow struct {
	provider provider.DataVolumeProvider
	actions  EnvironmentCreationActions
	ids      IDGenerator
	now      func() time.Time
}

func EnvironmentCreateDefinition(dataVolumes provider.DataVolumeProvider, actions EnvironmentCreationActions, ids IDGenerator, now func() time.Time) restate.ServiceDefinition {
	workflow := &environmentCreateWorkflow{provider: dataVolumes, actions: actions, ids: ids, now: now}
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
			ServicesID: workflow.ids.NewID(), CacheID: workflow.ids.NewID(), CreatedAt: workflow.now(),
		}, nil
	}, restate.WithName("reserve-state-identities"))
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
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(workflow.actions.CompleteEnvironmentCreation(runCtx, input.OperationID, workflow.now()))
	}, restate.WithName("complete-projection")); err != nil {
		return EnvironmentCreateOutput{}, err
	}
	return EnvironmentCreateOutput{DataVolumeProviderID: inventory.DataVolumeProviderID}, nil
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
