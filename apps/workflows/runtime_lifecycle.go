package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

const (
	CreditsPolicyBlocked  = "CREDITS_POLICY_BLOCKED"
	RuntimeReplaceService = "RuntimeReplace"
)

type RuntimeOperationState struct {
	OwnerUserID            string                 `json:"ownerUserId"`
	Runtime                domain.RuntimeSnapshot `json:"runtime"`
	DataVolumeProviderID   string                 `json:"dataVolumeProviderId"`
	ComputeUsageIntervalID string                 `json:"computeUsageIntervalId,omitempty"`
}

type RuntimeLifecycleActions interface {
	LoadRuntimeOperation(context.Context, domain.RuntimeOperationDispatch, string, time.Time) (RuntimeOperationState, error)
	PersistRuntimeTransition(context.Context, string, int64, domain.RuntimeSnapshot) error
	CompleteRuntimeOperation(context.Context, string, time.Time) error
	RecordRuntimeFailure(context.Context, string, string, time.Time) error
}

type RuntimeOperationSender interface {
	SendRuntimeOperation(context.Context, domain.RuntimeOperationDispatch) error
}

type RuntimeStartActions interface {
	RuntimeLifecycleActions
	RecordRuntimeStartDecision(context.Context, string, string, string) error
}

type RuntimeStopActions interface {
	RuntimeLifecycleActions
	RecordRuntimeStopReason(context.Context, string, domain.RuntimeStopReason) error
	RecordRuntimeStopSnapshot(context.Context, string, AutoStopObservation) error
}

type RuntimeDataVolumeRequest struct {
	OwnerUserID          string
	EnvironmentID        string
	RuntimeID            string
	Region               string
	AvailabilityZone     string
	DataVolumeProviderID string
}

type RuntimeDataVolumeVerifier interface {
	VerifyRuntimeDataVolume(context.Context, RuntimeDataVolumeRequest) error
}

type PromotedImageSource interface {
	CurrentPromotedImage(context.Context, string) (string, error)
}

type CreditBalanceSource interface {
	CreditBalance(context.Context, string) (dbstore.CreditBalanceProjection, error)
}

type ComputeUsageStore interface {
	OpenComputeUsageInterval(context.Context, dbstore.OpenComputeUsageIntervalInput) (dbstore.ComputeUsageInterval, error)
	CloseComputeUsageInterval(context.Context, dbstore.CloseComputeUsageIntervalInput) (billing.CreditTransaction, error)
}

type RuntimeGuestReadinessRequest struct {
	OwnerUserID   string
	EnvironmentID string
	RuntimeID     string
	ProviderID    string
	PrivateIPv4   string
}

type RuntimeGuestReadiness struct {
	BootID      string
	PrivateIPv4 string
	DataMounted bool
}

type RuntimeGuestReadinessSource interface {
	WaitForRuntimeReady(context.Context, RuntimeGuestReadinessRequest) (RuntimeGuestReadiness, error)
}

type RuntimeSSHKeyReconciler interface {
	ReconcileRuntimeSSHKeys(context.Context, RuntimeGuestReadinessRequest) error
}

type RuntimeGuestShutdownPreparer interface {
	PrepareRuntimeShutdown(context.Context, RuntimeGuestReadinessRequest) error
}

type runtimeProviderOutcome struct {
	Runtime     provider.Runtime `json:"runtime"`
	FailureCode string           `json:"failureCode,omitempty"`
	Failure     string           `json:"failure,omitempty"`
}

func runtimeLifecycleRequest(state RuntimeOperationState) (provider.RuntimeLifecycleRequest, error) {
	runtime := state.Runtime
	if runtime.ProviderInstanceRef == nil || strings.TrimSpace(*runtime.ProviderInstanceRef) == "" ||
		strings.TrimSpace(state.DataVolumeProviderID) == "" {
		return provider.RuntimeLifecycleRequest{}, errors.New("Runtime provider instance and data volume are required")
	}
	return provider.RuntimeLifecycleRequest{
		RuntimeSpec: provider.RuntimeSpec{
			RuntimeID: runtime.ID, EnvironmentID: runtime.EnvironmentID, Sequence: runtime.Sequence,
			Region: runtime.Region, AvailabilityZone: runtime.AvailabilityZone, RuntimePreset: runtime.RuntimePreset,
			ImageVersion: runtime.ImageVersion, DataVolumeProviderID: state.DataVolumeProviderID,
		},
		ProviderID: *runtime.ProviderInstanceRef,
	}, nil
}

func validateRuntimeOperationInput(input domain.RuntimeOperationDispatch, expected domain.OperationType, state RuntimeOperationState) error {
	if input.OperationID == "" || input.OperationType != expected || input.EnvironmentID == "" || input.RuntimeID == "" || input.OwnerUserID == "" {
		return errors.New("Runtime Operation dispatch is incomplete or has the wrong type")
	}
	if state.OwnerUserID != input.OwnerUserID || state.Runtime.ID != input.RuntimeID || state.Runtime.EnvironmentID != input.EnvironmentID {
		return errors.New("Runtime Operation target does not belong to the Environment")
	}
	return nil
}

func validateProviderRuntime(observed provider.Runtime, request provider.RuntimeLifecycleRequest, expected provider.RuntimeState) error {
	if observed.RuntimeSpec != request.RuntimeSpec || observed.ProviderID != request.ProviderID || observed.State != expected {
		return fmt.Errorf("Runtime provider observation diverged: state is %q, want %q", observed.State, expected)
	}
	return nil
}

func providerDivergenceOutcome(err error) runtimeProviderOutcome {
	return runtimeProviderOutcome{FailureCode: string(provider.ErrorCodeResourceDiverged), Failure: err.Error()}
}

func providerOutcome(runtime provider.Runtime, err error) (runtimeProviderOutcome, error) {
	if err == nil {
		return runtimeProviderOutcome{Runtime: runtime}, nil
	}
	var classified interface{ Transient() bool }
	if errors.As(err, &classified) && classified.Transient() {
		return runtimeProviderOutcome{}, err
	}
	code := "PROVIDER_FAILED"
	var providerError *provider.Error
	if errors.As(err, &providerError) {
		code = string(providerError.Code)
	}
	return runtimeProviderOutcome{FailureCode: code, Failure: err.Error()}, nil
}

func persistRuntimeTransition(ctx restate.WorkflowContext, actions RuntimeLifecycleActions, operationID, name string, before domain.RuntimeSnapshot, after domain.Runtime) (domain.RuntimeSnapshot, error) {
	next := after.Snapshot()
	result, err := restate.Run(ctx, func(runCtx restate.RunContext) (domain.RuntimeSnapshot, error) {
		if err := actions.PersistRuntimeTransition(runCtx, operationID, before.Version, next); err != nil {
			return domain.RuntimeSnapshot{}, classifyDurableError(err)
		}
		return next, nil
	}, restate.WithName(name))
	return result, err
}

func markRuntimeProviderFailure(ctx restate.WorkflowContext, actions RuntimeLifecycleActions, operationID string, current domain.RuntimeSnapshot, outcome runtimeProviderOutcome, now func() time.Time) error {
	runtime, err := domain.RestoreRuntime(current)
	if err != nil {
		return restate.ToTerminalError(err)
	}
	failed, err := runtime.MarkError(domain.RuntimeStateObservation{
		ProviderInstanceRef: *current.ProviderInstanceRef, ExpectedVersion: current.Version, ObservedAt: now(),
	})
	if err != nil {
		return restate.ToTerminalError(err)
	}
	if _, err := persistRuntimeTransition(ctx, actions, operationID, "mark-runtime-error", current, failed); err != nil {
		return err
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(actions.RecordRuntimeFailure(runCtx, operationID, outcome.FailureCode, now()))
	}, restate.WithName("fail-runtime-operation")); err != nil {
		return err
	}
	return restate.TerminalErrorf("%s: %s", outcome.FailureCode, outcome.Failure)
}

func dataVolumeRequest(input domain.RuntimeOperationDispatch, state RuntimeOperationState) RuntimeDataVolumeRequest {
	return RuntimeDataVolumeRequest{
		OwnerUserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID,
		Region: state.Runtime.Region, AvailabilityZone: state.Runtime.AvailabilityZone,
		DataVolumeProviderID: state.DataVolumeProviderID,
	}
}

func (client *Client) SendRuntimeOperation(ctx context.Context, input domain.RuntimeOperationDispatch) error {
	var service string
	switch input.OperationType {
	case domain.OperationRuntimeStart:
		service = RuntimeStartService
	case domain.OperationRuntimeStop:
		service = RuntimeStopService
	case domain.OperationRuntimeReplace:
		service = RuntimeReplaceService
	default:
		return fmt.Errorf("send Runtime Operation: unsupported type %q", input.OperationType)
	}
	_, err := ingress.WorkflowSend[domain.RuntimeOperationDispatch](
		client.ingress, service, input.OperationID, RunHandler,
	).Send(ctx, input)
	return err
}
