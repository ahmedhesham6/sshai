package workflows

import (
	"fmt"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
)

const RuntimeStartService = "RuntimeStart"

type RuntimeStartDependencies struct {
	Provider             provider.RuntimeProvider
	Actions              RuntimeStartActions
	DataVolumes          RuntimeDataVolumeVerifier
	Credits              CreditBalanceSource
	Images               PromotedImageSource
	Usage                ComputeUsageStore
	Guest                RuntimeGuestReadinessSource
	SSHKeys              RuntimeSSHKeyReconciler
	Managed              RuntimeManagedConfigurationReconciler
	AutoStop             RuntimeAutoStopController
	IDs                  IDGenerator
	Now                  func() time.Time
	ProviderPollInterval time.Duration
	ProviderPollTimeout  time.Duration
}

type RuntimeStartOutput struct {
	RuntimeID    string `json:"runtimeId"`
	PrivateRoute string `json:"privateRoute,omitempty"`
}

type runtimeStartWorkflow struct{ dependencies RuntimeStartDependencies }

type runtimeComputeUsageSeed struct {
	ID string `json:"id"`
}

func RuntimeStartDefinition(dependencies RuntimeStartDependencies) restate.ServiceDefinition {
	workflow := &runtimeStartWorkflow{dependencies: dependencies}
	return restate.NewWorkflow(RuntimeStartService).Handler(RunHandler, restate.NewWorkflowHandler(workflow.Run))
}

func (workflow *runtimeStartWorkflow) Run(ctx restate.WorkflowContext, input domain.RuntimeOperationDispatch) (RuntimeStartOutput, error) {
	if restate.Key(ctx) != input.OperationID {
		return RuntimeStartOutput{}, restate.TerminalErrorf("workflow key does not match Operation ID")
	}
	dependencies := workflow.dependencies
	state, err := restate.Run(ctx, func(runCtx restate.RunContext) (RuntimeOperationState, error) {
		state, err := dependencies.Actions.LoadRuntimeOperation(runCtx, input, runCtx.Request().ID, dependencies.Now())
		return state, classifyDurableError(err)
	}, restate.WithName("lock-runtime-operation"))
	if err != nil {
		return RuntimeStartOutput{}, err
	}
	if err := validateRuntimeOperationInput(input, domain.OperationRuntimeStart, state); err != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	if state.Runtime.Status == domain.RuntimeReady {
		if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
			return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
		}
		return RuntimeStartOutput{RuntimeID: input.RuntimeID, PrivateRoute: *state.Runtime.PrivateAddress}, nil
	}
	if state.Runtime.Status != domain.RuntimeStopped {
		message := fmt.Sprintf("Runtime status is %q, want %q", state.Runtime.Status, domain.RuntimeStopped)
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, message, dependencies.Now)
	}
	if dependencies.Managed == nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, "managed configuration reconciler is required", dependencies.Now)
	}
	request, requestErr := runtimeLifecycleRequest(state)
	if requestErr != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, requestErr.Error(), dependencies.Now)
	}
	observed, err := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		runtime, err := dependencies.Provider.ObserveRuntime(runCtx, request)
		return providerOutcome(runtime, err)
	}, restate.WithName("verify-stopped-runtime"))
	if err != nil {
		return RuntimeStartOutput{}, err
	}
	if observed.Failure != "" {
		return RuntimeStartOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, observed, dependencies.Now)
	}
	if err := validateProviderRuntime(observed.Runtime, request, provider.RuntimeStateStopped); err != nil {
		return RuntimeStartOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.DataVolumes.VerifyRuntimeDataVolume(runCtx, dataVolumeRequest(input, state)))
	}, restate.WithName("verify-data-volume")); err != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	balance, err := restate.Run(ctx, func(runCtx restate.RunContext) (dbstore.CreditBalanceProjection, error) {
		balance, err := dependencies.Credits.CreditBalance(runCtx, input.OwnerUserID)
		return balance, classifyDurableError(err)
	}, restate.WithName("check-credit-balance"))
	if err != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	if balance.UserID != input.OwnerUserID {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, "Credit Balance belongs to another User", dependencies.Now)
	}
	if balance.Credits <= 0 {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, CreditsPolicyBlocked, "Credit Balance must be positive to start a Runtime", dependencies.Now)
	}
	promotedImage, err := restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
		image, err := dependencies.Images.CurrentPromotedImage(runCtx, state.Runtime.Region)
		return image, classifyDurableError(err)
	}, restate.WithName("resolve-promoted-image"))
	if err != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	if promotedImage == "" {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, "promoted image is required", dependencies.Now)
	}
	if promotedImage != state.Runtime.ImageVersion {
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
			return classifyDurableError(dependencies.Actions.RecordRuntimeStartDecision(runCtx, input.OperationID, "replace", promotedImage))
		}, restate.WithName("record-replace-on-start")); err != nil {
			return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
		}
		// S4 seam: replace this terminal outcome with durable runtime.replace
		// fulfillment once that workflow exists. Never acknowledge readiness here.
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, ReplaceRequired, "a newer promoted image requires Runtime replacement", dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.RecordRuntimeStartDecision(runCtx, input.OperationID, "start", promotedImage))
	}, restate.WithName("record-start-decision")); err != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	runtime, restoreErr := domain.RestoreRuntime(state.Runtime)
	if restoreErr != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, restoreErr.Error(), dependencies.Now)
	}
	starting, transitionErr := runtime.BeginStart(dependencies.Now())
	if transitionErr != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, transitionErr.Error(), dependencies.Now)
	}
	state.Runtime, transitionErr = persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "begin-runtime-start", state.Runtime, starting)
	if transitionErr != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, transitionErr.Error(), dependencies.Now)
	}
	started, err := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		runtime, err := dependencies.Provider.StartRuntime(runCtx, request)
		outcome, outcomeErr := providerOutcome(runtime, err)
		return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
	}, restate.WithName("start-runtime-provider"))
	if err != nil {
		return RuntimeStartOutput{}, err
	}
	if started.Failure != "" {
		return RuntimeStartOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, started, dependencies.Now)
	}
	if err := validateProviderRuntimeIdentity(started.Runtime, request); err != nil || started.Runtime.State != provider.RuntimeStatePending && started.Runtime.State != provider.RuntimeStateRunning {
		if err == nil {
			err = fmt.Errorf("Runtime provider observation diverged: state is %q, want %q or %q", started.Runtime.State, provider.RuntimeStatePending, provider.RuntimeStateRunning)
		}
		return RuntimeStartOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
	}
	usageSeed, err := restate.Run(ctx, func(restate.RunContext) (runtimeComputeUsageSeed, error) {
		return runtimeComputeUsageSeed{ID: dependencies.IDs.NewID()}, nil
	}, restate.WithName("reserve-compute-usage-identity"))
	if err != nil {
		return RuntimeStartOutput{}, markRuntimeErrorAndFail(ctx, dependencies.Actions, input.OperationID, state.Runtime, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	interval, err := restate.Run(ctx, func(runCtx restate.RunContext) (dbstore.ComputeUsageInterval, error) {
		interval, err := dependencies.Usage.OpenComputeUsageInterval(runCtx, dbstore.OpenComputeUsageIntervalInput{
			ID: usageSeed.ID, UserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID,
			RuntimeID: input.RuntimeID, StartedAt: started.ObservedAt,
		})
		return interval, classifyDurableError(err)
	}, restate.WithName("open-compute-usage"))
	if err != nil {
		return RuntimeStartOutput{}, markRuntimeErrorAndFail(ctx, dependencies.Actions, input.OperationID, state.Runtime, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	if interval.UserID != input.OwnerUserID || interval.EnvironmentID != input.EnvironmentID || interval.RuntimeID != input.RuntimeID {
		return RuntimeStartOutput{}, markRuntimeErrorAndFail(ctx, dependencies.Actions, input.OperationID, state.Runtime, RuntimeStartFailed, "Compute Usage Interval ownership diverged", dependencies.Now)
	}
	running, waitErr := waitForProviderState(ctx, dependencies.Provider, request, started, provider.RuntimeStateRunning, provider.RuntimeStatePending, "wait-runtime-running", dependencies.ProviderPollInterval, dependencies.ProviderPollTimeout, dependencies.Now)
	if waitErr != nil {
		return RuntimeStartOutput{}, waitErr
	}
	if running.Failure != "" {
		return RuntimeStartOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, running, dependencies.Now)
	}
	guestRequest := RuntimeGuestReadinessRequest{
		OwnerUserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID,
		ProviderID: request.ProviderID, PrivateIPv4: running.Runtime.PrivateIPv4,
	}
	ready, err := restate.Run(ctx, func(runCtx restate.RunContext) (RuntimeGuestReadiness, error) {
		ready, err := dependencies.Guest.WaitForRuntimeReady(runCtx, guestRequest)
		return ready, classifyDurableError(err)
	}, restate.WithName("wait-for-guest-readiness"))
	if err != nil {
		return RuntimeStartOutput{}, markRuntimeErrorAndFail(ctx, dependencies.Actions, input.OperationID, state.Runtime, GuestNotReady, err.Error(), dependencies.Now)
	}
	if !ready.DataMounted || ready.BootID == "" || ready.PrivateIPv4 == "" || ready.PrivateIPv4 != running.Runtime.PrivateIPv4 {
		return RuntimeStartOutput{}, markRuntimeErrorAndFail(ctx, dependencies.Actions, input.OperationID, state.Runtime, GuestNotReady, "current boot and mounted data are required", dependencies.Now)
	}
	guestRequest.PrivateIPv4 = ready.PrivateIPv4
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.SSHKeys.ReconcileRuntimeSSHKeys(runCtx, guestRequest))
	}, restate.WithName("reconcile-runtime-ssh-keys")); err != nil {
		return RuntimeStartOutput{}, markRuntimeErrorAndFail(ctx, dependencies.Actions, input.OperationID, state.Runtime, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Managed.ReconcileRuntimeManagedConfiguration(runCtx, guestRequest))
	}, restate.WithName("reconcile-runtime-managed-configuration")); err != nil {
		return RuntimeStartOutput{}, markRuntimeErrorAndFail(ctx, dependencies.Actions, input.OperationID, state.Runtime, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	runtime, restoreErr = domain.RestoreRuntime(state.Runtime)
	if restoreErr != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, restoreErr.Error(), dependencies.Now)
	}
	markedReady, transitionErr := runtime.MarkReady(domain.RuntimeReadinessObservation{
		ProviderInstanceRef: request.ProviderID, BootID: ready.BootID, PrivateAddress: ready.PrivateIPv4,
		ExpectedVersion: state.Runtime.Version, ObservedAt: dependencies.Now(),
	})
	if transitionErr != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, transitionErr.Error(), dependencies.Now)
	}
	state.Runtime, transitionErr = persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "mark-runtime-ready", state.Runtime, markedReady)
	if transitionErr != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, transitionErr.Error(), dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.AutoStop.ResumeAutoStop(runCtx, input.EnvironmentID, input.RuntimeID))
	}, restate.WithName("resume-auto-stop")); err != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
		return RuntimeStartOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeStartFailed, err.Error(), dependencies.Now)
	}
	return RuntimeStartOutput{RuntimeID: input.RuntimeID, PrivateRoute: ready.PrivateIPv4}, nil
}

func completeRuntimeOperation(ctx restate.WorkflowContext, actions RuntimeLifecycleActions, operationID string, now func() time.Time) error {
	return restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(actions.CompleteRuntimeOperation(runCtx, operationID, now()))
	}, restate.WithName("complete-runtime-operation"))
}
