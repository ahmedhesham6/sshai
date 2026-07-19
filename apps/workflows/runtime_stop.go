package workflows

import (
	"fmt"
	"log/slog"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
)

const RuntimeStopService = "RuntimeStop"

type RuntimeStopDependencies struct {
	Provider                   provider.RuntimeProvider
	Actions                    RuntimeStopActions
	DataVolumes                RuntimeDataVolumeVerifier
	Snapshots                  AutoStopSnapshotSource
	Guest                      RuntimeGuestShutdownPreparer
	Usage                      ComputeUsageStore
	AutoStop                   RuntimeAutoStopController
	Now                        func() time.Time
	ProviderPollInterval       time.Duration
	ProviderPollTimeout        time.Duration
	SnapshotPollTimeout        time.Duration
	SnapshotPollInitialBackoff time.Duration
	SnapshotPollMaxBackoff     time.Duration
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
	storedStopped := state.Runtime.Status == domain.RuntimeStopped
	if !storedStopped && state.Runtime.Status != domain.RuntimeReady {
		message := fmt.Sprintf("Runtime status is %q, want %q or %q", state.Runtime.Status, domain.RuntimeReady, domain.RuntimeStopped)
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, message, dependencies.Now)
	}
	request, requestErr := runtimeLifecycleRequest(state)
	if requestErr != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, requestErr.Error(), dependencies.Now)
	}
	var storedStoppedObservation runtimeProviderOutcome
	if storedStopped {
		storedStoppedObservation, err = restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
			runtime, err := dependencies.Provider.ObserveRuntime(runCtx, request)
			outcome, outcomeErr := providerOutcome(runtime, err)
			return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
		}, restate.WithName("verify-stored-stopped-runtime"))
		if err != nil {
			return RuntimeStopOutput{}, err
		}
		if storedStoppedObservation.Failure != "" {
			return RuntimeStopOutput{}, failRuntimeOperationForProviderOutcome(ctx, dependencies.Actions, dependencies.Usage, input.OperationID, state.Runtime, state.ComputeUsageIntervalID, storedStoppedObservation, RuntimeStopFailed, "close-compute-usage-after-stored-stop-observation", dbstore.ComputeUsageClosedByRuntimeStop, dependencies.Now)
		}
		if err := validateProviderRuntimeIdentity(storedStoppedObservation.Runtime, request); err != nil {
			return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
		}
		if storedStoppedObservation.Runtime.State == provider.RuntimeStateStopped {
			return completeObservedStoppedRuntime(ctx, dependencies, input, state, storedStoppedObservation)
		}
		if storedStoppedObservation.Runtime.State == provider.RuntimeStateTerminated {
			outcome := providerStateDivergenceOutcome(storedStoppedObservation, fmt.Errorf("Runtime provider observation diverged: state is %q, want %q", storedStoppedObservation.Runtime.State, provider.RuntimeStateStopped))
			return RuntimeStopOutput{}, failRuntimeOperationForProviderOutcome(ctx, dependencies.Actions, dependencies.Usage, input.OperationID, state.Runtime, state.ComputeUsageIntervalID, outcome, RuntimeStopFailed, "close-compute-usage-after-stored-stop-observation", dbstore.ComputeUsageClosedByRuntimeStop, dependencies.Now)
		}
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.AutoStop.SuppressAutoStop(runCtx, input.EnvironmentID, input.RuntimeID))
	}, restate.WithName("suppress-auto-stop")); err != nil {
		return RuntimeStopOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	failWhileReady := func(code, message string) error {
		return resumeAutoStopAndFailRuntimeStop(ctx, dependencies, input, state, code, message)
	}
	freshAfter := dependencies.Now()
	read, refreshErr := refreshAutoStopSnapshot(ctx, dependencies.Snapshots, AutoStopRefreshRequest{
		EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID, FreshAfter: freshAfter,
	}, autoStopSnapshotPolling{
		timeout: dependencies.SnapshotPollTimeout, initialBackoff: dependencies.SnapshotPollInitialBackoff,
		maxBackoff: dependencies.SnapshotPollMaxBackoff,
	}, "request-activity-snapshot")
	if refreshErr != nil {
		return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, refreshErr.Error())
	}
	if read.ReferenceUnavailable {
		return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, dbstore.ErrReferenceNotOwned.Error())
	}
	snapshot := read.Observation
	if snapshot.RuntimeID != input.RuntimeID || snapshot.Snapshot == nil || snapshot.Snapshot.RuntimeID != input.RuntimeID {
		return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, "Activity Snapshot belongs to another Runtime")
	}
	if snapshot.FreshAfter.IsZero() || snapshot.Snapshot.ObservedAt.Before(snapshot.FreshAfter) {
		return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, "Activity Snapshot is stale")
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.RecordRuntimeStopSnapshot(runCtx, input.OperationID, snapshot))
	}, restate.WithName("record-stop-activity-snapshot")); err != nil {
		return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, err.Error())
	}
	guestRequest := RuntimeGuestReadinessRequest{
		OwnerUserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID,
		ProviderID: request.ProviderID,
	}
	if state.Runtime.PrivateAddress != nil {
		guestRequest.PrivateIPv4 = *state.Runtime.PrivateAddress
	} else if storedStopped {
		guestRequest.PrivateIPv4 = storedStoppedObservation.Runtime.PrivateIPv4
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Guest.PrepareRuntimeShutdown(runCtx, guestRequest))
	}, restate.WithName("prepare-guest-shutdown")); err != nil {
		return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, err.Error())
	}
	var runtime domain.Runtime
	if !storedStopped {
		var restoreErr error
		runtime, restoreErr = domain.RestoreRuntime(state.Runtime)
		if restoreErr != nil {
			return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, restoreErr.Error())
		}
		stopping, transitionErr := runtime.BeginStop(dependencies.Now())
		if transitionErr != nil {
			return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, transitionErr.Error())
		}
		nextRuntime, transitionErr := persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "begin-runtime-stop", state.Runtime, stopping)
		if transitionErr != nil {
			return RuntimeStopOutput{}, failWhileReady(RuntimeStopFailed, transitionErr.Error())
		}
		state.Runtime = nextRuntime
	}
	stopped, err := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		runtime, err := dependencies.Provider.StopRuntime(runCtx, request)
		outcome, outcomeErr := providerOutcome(runtime, err)
		return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
	}, restate.WithName("stop-runtime-provider"))
	if err != nil {
		return RuntimeStopOutput{}, err
	}
	if stopped.Failure != "" {
		return RuntimeStopOutput{}, failRuntimeOperationForProviderOutcome(ctx, dependencies.Actions, dependencies.Usage, input.OperationID, state.Runtime, state.ComputeUsageIntervalID, stopped, RuntimeStopFailed, "close-compute-usage-after-stop-provider", dbstore.ComputeUsageClosedByRuntimeStop, dependencies.Now)
	}
	if err := validateProviderRuntimeIdentity(stopped.Runtime, request); err != nil {
		return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
	}
	if stopped.Runtime.State == provider.RuntimeStateTerminated {
		outcome := providerStateDivergenceOutcome(stopped, fmt.Errorf("Runtime provider observation diverged: state is %q, want %q, %q, or %q", stopped.Runtime.State, provider.RuntimeStateRunning, provider.RuntimeStateStopping, provider.RuntimeStateStopped))
		return RuntimeStopOutput{}, failRuntimeOperationForProviderOutcome(ctx, dependencies.Actions, dependencies.Usage, input.OperationID, state.Runtime, state.ComputeUsageIntervalID, outcome, RuntimeStopFailed, "close-compute-usage-after-stop-provider", dbstore.ComputeUsageClosedByRuntimeStop, dependencies.Now)
	}
	if stopped.Runtime.State != provider.RuntimeStateRunning && stopped.Runtime.State != provider.RuntimeStateStopping && stopped.Runtime.State != provider.RuntimeStateStopped {
		err := fmt.Errorf("Runtime provider observation diverged: state is %q, want %q, %q, or %q", stopped.Runtime.State, provider.RuntimeStateRunning, provider.RuntimeStateStopping, provider.RuntimeStateStopped)
		return RuntimeStopOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
	}
	observed, waitErr := waitForProviderState(ctx, dependencies.Provider, request, stopped, provider.RuntimeStateStopped, provider.RuntimeStateStopping, "wait-runtime-stopped", dependencies.ProviderPollInterval, dependencies.ProviderPollTimeout, dependencies.Now, provider.RuntimeStateRunning)
	if waitErr != nil {
		return RuntimeStopOutput{}, waitErr
	}
	if observed.Failure != "" {
		return RuntimeStopOutput{}, failRuntimeOperationForProviderOutcome(ctx, dependencies.Actions, dependencies.Usage, input.OperationID, state.Runtime, state.ComputeUsageIntervalID, observed, RuntimeStopFailed, "close-compute-usage-after-stop-observation", dbstore.ComputeUsageClosedByRuntimeStop, dependencies.Now)
	}
	if err := finalizeObservedStoppedRuntime(ctx, dependencies, input, state, observed, storedStopped); err != nil {
		return RuntimeStopOutput{}, err
	}
	return RuntimeStopOutput{RuntimeID: input.RuntimeID, Reason: input.StopReason}, nil
}

func completeObservedStoppedRuntime(ctx restate.WorkflowContext, dependencies RuntimeStopDependencies, input domain.RuntimeOperationDispatch, state RuntimeOperationState, observed runtimeProviderOutcome) (RuntimeStopOutput, error) {
	if err := finalizeObservedStoppedRuntime(ctx, dependencies, input, state, observed, true); err != nil {
		return RuntimeStopOutput{}, err
	}
	return RuntimeStopOutput{RuntimeID: input.RuntimeID, Reason: input.StopReason}, nil
}

func finalizeObservedStoppedRuntime(ctx restate.WorkflowContext, dependencies RuntimeStopDependencies, input domain.RuntimeOperationDispatch, state RuntimeOperationState, observed runtimeProviderOutcome, storedStopped bool) error {
	verifyName := "verify-data-volume"
	closeName := "close-compute-usage-after-stop-observation"
	if storedStopped {
		verifyName = "verify-stopped-data-volume"
		closeName = "close-compute-usage-after-stored-stop-observation"
	}
	var postStopErr error
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.DataVolumes.VerifyRuntimeDataVolume(runCtx, dataVolumeRequest(input, state)))
	}, restate.WithName(verifyName)); err != nil {
		postStopErr = err
	}
	if state.ComputeUsageIntervalID == "" {
		if storedStopped {
			slog.InfoContext(ctx, "Compute Usage already reconciled for stored stopped Runtime",
				"operation_id", input.OperationID, "runtime_id", input.RuntimeID)
		} else if postStopErr == nil {
			postStopErr = fmt.Errorf("open Compute Usage Interval is required")
		}
	} else {
		if err := closeComputeUsageForProviderOutcome(ctx, dependencies.Usage, state.ComputeUsageIntervalID, observed, closeName, dbstore.ComputeUsageClosedByRuntimeStop); err != nil {
			if postStopErr == nil {
				postStopErr = err
			}
		}
	}
	if !storedStopped {
		runtime, err := domain.RestoreRuntime(state.Runtime)
		if err != nil {
			return failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
		}
		markedStopped, err := runtime.MarkStopped(domain.RuntimeStateObservation{
			ProviderInstanceRef: *state.Runtime.ProviderInstanceRef, ExpectedVersion: state.Runtime.Version, ObservedAt: dependencies.Now(),
		})
		if err != nil {
			return failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
		}
		if _, err := persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "mark-runtime-stopped", state.Runtime, markedStopped); err != nil {
			return failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
		}
	}
	if postStopErr != nil {
		return failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, postStopErr.Error(), dependencies.Now)
	}
	if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
		return failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStopFailed, err.Error(), dependencies.Now)
	}
	return nil
}

func resumeAutoStopAndFailRuntimeStop(ctx restate.WorkflowContext, dependencies RuntimeStopDependencies, input domain.RuntimeOperationDispatch, state RuntimeOperationState, code, message string) error {
	if state.Runtime.Status == domain.RuntimeReady || state.Runtime.Status == domain.RuntimeStopped {
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
			return classifyDurableError(dependencies.AutoStop.ResumeAutoStop(runCtx, input.EnvironmentID, input.RuntimeID))
		}, restate.WithName("resume-auto-stop-after-failed-stop")); err != nil {
			return err
		}
	}
	return failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, code, message, dependencies.Now)
}
