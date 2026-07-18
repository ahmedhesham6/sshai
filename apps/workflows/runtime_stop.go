package workflows

import (
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
)

const RuntimeStopService = "RuntimeStop"

type RuntimeStopDependencies struct {
	Provider    provider.RuntimeProvider
	Actions     RuntimeStopActions
	DataVolumes RuntimeDataVolumeVerifier
	Snapshots   AutoStopSnapshotSource
	Guest       RuntimeGuestShutdownPreparer
	Usage       ComputeUsageStore
	AutoStop    RuntimeAutoStopController
	Now         func() time.Time
}

type RuntimeStopOutput struct {
	RuntimeID string                   `json:"runtimeId"`
	Reason    domain.RuntimeStopReason `json:"reason"`
}

type runtimeStopWorkflow struct{ dependencies RuntimeStopDependencies }

func RuntimeStopDefinition(dependencies RuntimeStopDependencies) restate.ServiceDefinition {
	workflow := &runtimeStopWorkflow{dependencies: dependencies}
	return restate.NewWorkflow(RuntimeStopService).Handler(RunHandler, restate.NewWorkflowHandler(workflow.Run))
}

func (workflow *runtimeStopWorkflow) Run(ctx restate.WorkflowContext, input domain.RuntimeOperationDispatch) (RuntimeStopOutput, error) {
	if restate.Key(ctx) != input.OperationID {
		return RuntimeStopOutput{}, restate.TerminalErrorf("workflow key does not match Operation ID")
	}
	dependencies := workflow.dependencies
	state, err := restate.Run(ctx, func(runCtx restate.RunContext) (RuntimeOperationState, error) {
		state, err := dependencies.Actions.LoadRuntimeOperation(runCtx, input, runCtx.Request().ID, dependencies.Now())
		return state, classifyDurableError(err)
	}, restate.WithName("lock-runtime-operation"))
	if err != nil {
		return RuntimeStopOutput{}, err
	}
	if err := validateRuntimeOperationInput(input, domain.OperationRuntimeStop, state); err != nil {
		return RuntimeStopOutput{}, restate.ToTerminalError(err)
	}
	if !input.StopReason.Valid() {
		return RuntimeStopOutput{}, restate.TerminalErrorf("invalid Runtime stop reason %q", input.StopReason)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.RecordRuntimeStopReason(runCtx, input.OperationID, input.StopReason))
	}, restate.WithName("record-runtime-stop-reason")); err != nil {
		return RuntimeStopOutput{}, err
	}
	if state.Runtime.Status == domain.RuntimeStopped {
		if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
			return RuntimeStopOutput{}, err
		}
		return RuntimeStopOutput{RuntimeID: input.RuntimeID, Reason: input.StopReason}, nil
	}
	if state.Runtime.Status != domain.RuntimeReady {
		return RuntimeStopOutput{}, restate.TerminalErrorf("Runtime status is %q, want %q", state.Runtime.Status, domain.RuntimeReady)
	}
	request, requestErr := runtimeLifecycleRequest(state)
	if requestErr != nil {
		return RuntimeStopOutput{}, restate.ToTerminalError(requestErr)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.AutoStop.SuppressAutoStop(runCtx, input.EnvironmentID, input.RuntimeID))
	}, restate.WithName("suppress-auto-stop")); err != nil {
		return RuntimeStopOutput{}, err
	}
	snapshot, err := restate.Run(ctx, func(runCtx restate.RunContext) (AutoStopObservation, error) {
		observation, err := dependencies.Snapshots.RefreshAutoStop(runCtx, AutoStopRefreshRequest{
			EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID, FreshAfter: dependencies.Now(),
		})
		return observation, classifyDurableError(err)
	}, restate.WithName("request-activity-snapshot"))
	if err != nil {
		return RuntimeStopOutput{}, err
	}
	if snapshot.RuntimeID != input.RuntimeID || snapshot.Snapshot == nil || snapshot.Snapshot.RuntimeID != input.RuntimeID {
		return RuntimeStopOutput{}, restate.TerminalErrorf("Activity Snapshot belongs to another Runtime")
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.RecordRuntimeStopSnapshot(runCtx, input.OperationID, snapshot))
	}, restate.WithName("record-stop-activity-snapshot")); err != nil {
		return RuntimeStopOutput{}, err
	}
	guestRequest := RuntimeGuestReadinessRequest{
		OwnerUserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID,
		ProviderID: request.ProviderID,
	}
	if state.Runtime.PrivateAddress != nil {
		guestRequest.PrivateIPv4 = *state.Runtime.PrivateAddress
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Guest.PrepareRuntimeShutdown(runCtx, guestRequest))
	}, restate.WithName("prepare-guest-shutdown")); err != nil {
		return RuntimeStopOutput{}, err
	}
	runtime, restoreErr := domain.RestoreRuntime(state.Runtime)
	if restoreErr != nil {
		return RuntimeStopOutput{}, restate.ToTerminalError(restoreErr)
	}
	stopping, transitionErr := runtime.BeginStop(dependencies.Now())
	if transitionErr != nil {
		return RuntimeStopOutput{}, restate.ToTerminalError(transitionErr)
	}
	state.Runtime, transitionErr = persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "begin-runtime-stop", state.Runtime, stopping)
	if transitionErr != nil {
		return RuntimeStopOutput{}, transitionErr
	}
	stopped, err := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		runtime, err := dependencies.Provider.StopRuntime(runCtx, request)
		return providerOutcome(runtime, err)
	}, restate.WithName("stop-runtime-provider"))
	if err != nil {
		return RuntimeStopOutput{}, err
	}
	if stopped.Failure != "" {
		return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, stopped, dependencies.Now)
	}
	if err := validateProviderRuntime(stopped.Runtime, request, provider.RuntimeStateStopped); err != nil {
		return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
	}
	observed, err := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		runtime, err := dependencies.Provider.ObserveRuntime(runCtx, request)
		return providerOutcome(runtime, err)
	}, restate.WithName("verify-runtime-stopped"))
	if err != nil {
		return RuntimeStopOutput{}, err
	}
	if observed.Failure != "" {
		return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, observed, dependencies.Now)
	}
	if err := validateProviderRuntime(observed.Runtime, request, provider.RuntimeStateStopped); err != nil {
		return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.DataVolumes.VerifyRuntimeDataVolume(runCtx, dataVolumeRequest(input, state)))
	}, restate.WithName("verify-data-volume")); err != nil {
		return RuntimeStopOutput{}, err
	}
	if state.ComputeUsageIntervalID == "" {
		return RuntimeStopOutput{}, restate.TerminalErrorf("open Compute Usage Interval is required")
	}
	if _, err := restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
		transaction, err := dependencies.Usage.CloseComputeUsageInterval(runCtx, dbstore.CloseComputeUsageIntervalInput{
			IntervalID: state.ComputeUsageIntervalID, StoppedAt: dependencies.Now(), Source: dbstore.ComputeUsageClosedByRuntimeStop,
		})
		if err != nil {
			return "", classifyDurableError(err)
		}
		return transaction.ID(), nil
	}, restate.WithName("close-compute-usage")); err != nil {
		return RuntimeStopOutput{}, err
	}
	runtime, restoreErr = domain.RestoreRuntime(state.Runtime)
	if restoreErr != nil {
		return RuntimeStopOutput{}, restate.ToTerminalError(restoreErr)
	}
	markedStopped, transitionErr := runtime.MarkStopped(domain.RuntimeStateObservation{
		ProviderInstanceRef: request.ProviderID, ExpectedVersion: state.Runtime.Version, ObservedAt: dependencies.Now(),
	})
	if transitionErr != nil {
		return RuntimeStopOutput{}, restate.ToTerminalError(transitionErr)
	}
	if _, err := persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "mark-runtime-stopped", state.Runtime, markedStopped); err != nil {
		return RuntimeStopOutput{}, err
	}
	if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
		return RuntimeStopOutput{}, err
	}
	return RuntimeStopOutput{RuntimeID: input.RuntimeID, Reason: input.StopReason}, nil
}
