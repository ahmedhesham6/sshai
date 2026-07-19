package workflows

import (
	"fmt"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
)

const RuntimeStopService = "RuntimeStop"

type RuntimeStopDependencies struct {
	Provider             provider.RuntimeProvider
	Actions              RuntimeStopActions
	DataVolumes          RuntimeDataVolumeVerifier
	Snapshots            AutoStopSnapshotSource
	Guest                RuntimeGuestShutdownPreparer
	Usage                ComputeUsageStore
	AutoStop             RuntimeAutoStopController
	Now                  func() time.Time
	ProviderPollInterval time.Duration
	ProviderPollTimeout  time.Duration
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
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	if !input.StopReason.Valid() {
		message := fmt.Sprintf("invalid Runtime stop reason %q", input.StopReason)
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, message, dependencies.Now)
	}
	if err := validateRuntimeStopAudit(input); err != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.RecordRuntimeStopReason(runCtx, input.OperationID, input.StopReason))
	}, restate.WithName("record-runtime-stop-reason")); err != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	if input.StopAudit != nil {
		audit := *domain.CloneRuntimeStopAuditEvidence(input.StopAudit)
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
			return classifyDurableError(dependencies.Actions.RecordRuntimeStopAudit(runCtx, input.OperationID, audit))
		}, restate.WithName("record-runtime-stop-audit")); err != nil {
			return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
		}
	}
	if state.Runtime.Status == domain.RuntimeStopped {
		if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
			return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
		}
		return RuntimeStopOutput{RuntimeID: input.RuntimeID, Reason: input.StopReason}, nil
	}
	if state.Runtime.Status != domain.RuntimeReady {
		message := fmt.Sprintf("Runtime status is %q, want %q", state.Runtime.Status, domain.RuntimeReady)
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, message, dependencies.Now)
	}
	request, requestErr := runtimeLifecycleRequest(state)
	if requestErr != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, requestErr.Error(), dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.AutoStop.SuppressAutoStop(runCtx, input.EnvironmentID, input.RuntimeID))
	}, restate.WithName("suppress-auto-stop")); err != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	snapshot, err := restate.Run(ctx, func(runCtx restate.RunContext) (AutoStopObservation, error) {
		observation, err := dependencies.Snapshots.RefreshAutoStop(runCtx, AutoStopRefreshRequest{
			EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID, FreshAfter: dependencies.Now(),
		})
		return observation, classifyDurableError(err)
	}, restate.WithName("request-activity-snapshot"))
	if err != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	if snapshot.RuntimeID != input.RuntimeID || snapshot.Snapshot == nil || snapshot.Snapshot.RuntimeID != input.RuntimeID {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, "Activity Snapshot belongs to another Runtime", dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.RecordRuntimeStopSnapshot(runCtx, input.OperationID, snapshot))
	}, restate.WithName("record-stop-activity-snapshot")); err != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
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
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	runtime, restoreErr := domain.RestoreRuntime(state.Runtime)
	if restoreErr != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, restoreErr.Error(), dependencies.Now)
	}
	stopping, transitionErr := runtime.BeginStop(dependencies.Now())
	if transitionErr != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, transitionErr.Error(), dependencies.Now)
	}
	state.Runtime, transitionErr = persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "begin-runtime-stop", state.Runtime, stopping)
	if transitionErr != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, transitionErr.Error(), dependencies.Now)
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
	if err := validateProviderRuntimeIdentity(stopped.Runtime, request); err != nil || stopped.Runtime.State != provider.RuntimeStateStopping && stopped.Runtime.State != provider.RuntimeStateStopped {
		if err == nil {
			err = fmt.Errorf("Runtime provider observation diverged: state is %q, want %q or %q", stopped.Runtime.State, provider.RuntimeStateStopping, provider.RuntimeStateStopped)
		}
		return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
	}
	observed, waitErr := waitForProviderState(ctx, dependencies.Provider, request, stopped.Runtime, provider.RuntimeStateStopped, provider.RuntimeStateStopping, "wait-runtime-stopped", dependencies.ProviderPollInterval, dependencies.ProviderPollTimeout)
	if waitErr != nil {
		return RuntimeStopOutput{}, waitErr
	}
	if observed.Failure != "" {
		return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, observed, dependencies.Now)
	}
	var postStopCode, postStopMessage string
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.DataVolumes.VerifyRuntimeDataVolume(runCtx, dataVolumeRequest(input, state)))
	}, restate.WithName("verify-data-volume")); err != nil {
		postStopCode, postStopMessage = RuntimeStopFailed, err.Error()
	}
	if state.ComputeUsageIntervalID == "" {
		if postStopCode == "" {
			postStopCode, postStopMessage = RuntimeStopFailed, "open Compute Usage Interval is required"
		}
	} else {
		if _, err := restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
			transaction, err := dependencies.Usage.CloseComputeUsageInterval(runCtx, dbstore.CloseComputeUsageIntervalInput{
				IntervalID: state.ComputeUsageIntervalID, StoppedAt: dependencies.Now(), Source: dbstore.ComputeUsageClosedByRuntimeStop,
			})
			if err != nil {
				return "", classifyDurableError(err)
			}
			return transaction.ID(), nil
		}, restate.WithName("close-compute-usage")); err != nil && postStopCode == "" {
			postStopCode, postStopMessage = RuntimeStopFailed, err.Error()
		}
	}
	runtime, restoreErr = domain.RestoreRuntime(state.Runtime)
	if restoreErr != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, restoreErr.Error(), dependencies.Now)
	}
	markedStopped, transitionErr := runtime.MarkStopped(domain.RuntimeStateObservation{
		ProviderInstanceRef: request.ProviderID, ExpectedVersion: state.Runtime.Version, ObservedAt: dependencies.Now(),
	})
	if transitionErr != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, transitionErr.Error(), dependencies.Now)
	}
	if _, err := persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "mark-runtime-stopped", state.Runtime, markedStopped); err != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	if postStopCode != "" {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, postStopCode, postStopMessage, dependencies.Now)
	}
	if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	return RuntimeStopOutput{RuntimeID: input.RuntimeID, Reason: input.StopReason}, nil
}
