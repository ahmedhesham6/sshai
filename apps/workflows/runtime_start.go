package workflows

import (
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
)

const RuntimeStartService = "RuntimeStart"

type RuntimeStartDependencies struct {
	Provider    provider.RuntimeProvider
	Actions     RuntimeStartActions
	DataVolumes RuntimeDataVolumeVerifier
	Credits     CreditBalanceSource
	Images      PromotedImageSource
	Usage       ComputeUsageStore
	Guest       RuntimeGuestReadinessSource
	SSHKeys     RuntimeSSHKeyReconciler
	AutoStop    RuntimeAutoStopController
	Replace     RuntimeOperationSender
	IDs         IDGenerator
	Now         func() time.Time
}

type RuntimeStartOutput struct {
	RuntimeID         string `json:"runtimeId"`
	PrivateRoute      string `json:"privateRoute,omitempty"`
	ReplaceDispatched bool   `json:"replaceDispatched,omitempty"`
}

type runtimeStartWorkflow struct{ dependencies RuntimeStartDependencies }

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
		return RuntimeStartOutput{}, restate.ToTerminalError(err)
	}
	if state.Runtime.Status == domain.RuntimeReady {
		if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
			return RuntimeStartOutput{}, err
		}
		return RuntimeStartOutput{RuntimeID: input.RuntimeID, PrivateRoute: *state.Runtime.PrivateAddress}, nil
	}
	if state.Runtime.Status != domain.RuntimeStopped {
		return RuntimeStartOutput{}, restate.TerminalErrorf("Runtime status is %q, want %q", state.Runtime.Status, domain.RuntimeStopped)
	}
	request, requestErr := runtimeLifecycleRequest(state)
	if requestErr != nil {
		return RuntimeStartOutput{}, restate.ToTerminalError(requestErr)
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
		return RuntimeStartOutput{}, err
	}
	balance, err := restate.Run(ctx, func(runCtx restate.RunContext) (dbstore.CreditBalanceProjection, error) {
		balance, err := dependencies.Credits.CreditBalance(runCtx, input.OwnerUserID)
		return balance, classifyDurableError(err)
	}, restate.WithName("check-credit-balance"))
	if err != nil {
		return RuntimeStartOutput{}, err
	}
	if balance.UserID != input.OwnerUserID {
		return RuntimeStartOutput{}, restate.TerminalErrorf("Credit Balance belongs to another User")
	}
	if balance.Credits <= 0 {
		return RuntimeStartOutput{}, restate.TerminalErrorf("%s: Credit Balance must be positive to start a Runtime", CreditsPolicyBlocked)
	}
	promotedImage, err := restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
		image, err := dependencies.Images.CurrentPromotedImage(runCtx, state.Runtime.Region)
		return image, classifyDurableError(err)
	}, restate.WithName("resolve-promoted-image"))
	if err != nil {
		return RuntimeStartOutput{}, err
	}
	if promotedImage == "" {
		return RuntimeStartOutput{}, restate.TerminalErrorf("promoted image is required")
	}
	if promotedImage != state.Runtime.ImageVersion {
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
			return classifyDurableError(dependencies.Actions.RecordRuntimeStartDecision(runCtx, input.OperationID, "replace", promotedImage))
		}, restate.WithName("record-replace-on-start")); err != nil {
			return RuntimeStartOutput{}, err
		}
		replace := input
		replace.OperationType = domain.OperationRuntimeReplace
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
			return classifyDurableError(dependencies.Replace.SendRuntimeOperation(runCtx, replace))
		}, restate.WithName("dispatch-runtime-replace")); err != nil {
			return RuntimeStartOutput{}, err
		}
		return RuntimeStartOutput{RuntimeID: input.RuntimeID, ReplaceDispatched: true}, nil
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.RecordRuntimeStartDecision(runCtx, input.OperationID, "start", promotedImage))
	}, restate.WithName("record-start-decision")); err != nil {
		return RuntimeStartOutput{}, err
	}
	runtime, restoreErr := domain.RestoreRuntime(state.Runtime)
	if restoreErr != nil {
		return RuntimeStartOutput{}, restate.ToTerminalError(restoreErr)
	}
	starting, transitionErr := runtime.BeginStart(dependencies.Now())
	if transitionErr != nil {
		return RuntimeStartOutput{}, restate.ToTerminalError(transitionErr)
	}
	state.Runtime, transitionErr = persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "begin-runtime-start", state.Runtime, starting)
	if transitionErr != nil {
		return RuntimeStartOutput{}, transitionErr
	}
	started, err := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		runtime, err := dependencies.Provider.StartRuntime(runCtx, request)
		return providerOutcome(runtime, err)
	}, restate.WithName("start-runtime-provider"))
	if err != nil {
		return RuntimeStartOutput{}, err
	}
	if started.Failure != "" {
		return RuntimeStartOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, started, dependencies.Now)
	}
	if err := validateProviderRuntime(started.Runtime, request, provider.RuntimeStateRunning); err != nil {
		return RuntimeStartOutput{}, markRuntimeProviderFailure(ctx, dependencies.Actions, input.OperationID, state.Runtime, providerDivergenceOutcome(err), dependencies.Now)
	}
	interval, err := restate.Run(ctx, func(runCtx restate.RunContext) (dbstore.ComputeUsageInterval, error) {
		interval, err := dependencies.Usage.OpenComputeUsageInterval(runCtx, dbstore.OpenComputeUsageIntervalInput{
			ID: dependencies.IDs.NewID(), UserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID,
			RuntimeID: input.RuntimeID, StartedAt: dependencies.Now(),
		})
		return interval, classifyDurableError(err)
	}, restate.WithName("open-compute-usage"))
	if err != nil {
		return RuntimeStartOutput{}, err
	}
	if interval.UserID != input.OwnerUserID || interval.EnvironmentID != input.EnvironmentID || interval.RuntimeID != input.RuntimeID {
		return RuntimeStartOutput{}, restate.TerminalErrorf("Compute Usage Interval ownership diverged")
	}
	guestRequest := RuntimeGuestReadinessRequest{
		OwnerUserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID,
		ProviderID: request.ProviderID, PrivateIPv4: started.Runtime.PrivateIPv4,
	}
	ready, err := restate.Run(ctx, func(runCtx restate.RunContext) (RuntimeGuestReadiness, error) {
		ready, err := dependencies.Guest.WaitForRuntimeReady(runCtx, guestRequest)
		return ready, classifyDurableError(err)
	}, restate.WithName("wait-for-guest-readiness"))
	if err != nil {
		return RuntimeStartOutput{}, err
	}
	if !ready.DataMounted || ready.BootID == "" || ready.PrivateIPv4 == "" || ready.PrivateIPv4 != started.Runtime.PrivateIPv4 {
		return RuntimeStartOutput{}, restate.TerminalErrorf("GUEST_NOT_READY: current boot and mounted data are required")
	}
	guestRequest.PrivateIPv4 = ready.PrivateIPv4
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.SSHKeys.ReconcileRuntimeSSHKeys(runCtx, guestRequest))
	}, restate.WithName("reconcile-runtime-ssh-keys")); err != nil {
		return RuntimeStartOutput{}, err
	}
	runtime, restoreErr = domain.RestoreRuntime(state.Runtime)
	if restoreErr != nil {
		return RuntimeStartOutput{}, restate.ToTerminalError(restoreErr)
	}
	markedReady, transitionErr := runtime.MarkReady(domain.RuntimeReadinessObservation{
		ProviderInstanceRef: request.ProviderID, BootID: ready.BootID, PrivateAddress: ready.PrivateIPv4,
		ExpectedVersion: state.Runtime.Version, ObservedAt: dependencies.Now(),
	})
	if transitionErr != nil {
		return RuntimeStartOutput{}, restate.ToTerminalError(transitionErr)
	}
	state.Runtime, transitionErr = persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "mark-runtime-ready", state.Runtime, markedReady)
	if transitionErr != nil {
		return RuntimeStartOutput{}, transitionErr
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.AutoStop.ResumeAutoStop(runCtx, input.EnvironmentID, input.RuntimeID))
	}, restate.WithName("resume-auto-stop")); err != nil {
		return RuntimeStartOutput{}, err
	}
	if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
		return RuntimeStartOutput{}, err
	}
	return RuntimeStartOutput{RuntimeID: input.RuntimeID, PrivateRoute: ready.PrivateIPv4}, nil
}

func completeRuntimeOperation(ctx restate.WorkflowContext, actions RuntimeLifecycleActions, operationID string, now func() time.Time) error {
	return restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(actions.CompleteRuntimeOperation(runCtx, operationID, now()))
	}, restate.WithName("complete-runtime-operation"))
}
