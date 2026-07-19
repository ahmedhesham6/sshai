package workflows

import (
	"errors"
	"fmt"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	restate "github.com/restatedev/sdk-go"
)

type RuntimeReplaceDependencies struct {
	Provider             provider.RuntimeProvider
	Attachments          provider.RuntimeDataVolumeAttachmentObserver
	Actions              RuntimeReplaceActions
	DataVolumes          RuntimeDataVolumeVerifier
	Images               PromotedImageSource
	Usage                ComputeUsageStore
	Guest                RuntimeGuestReadinessSource
	HostIdentity         RuntimeSSHHostIdentityReconciler
	SSHKeys              RuntimeSSHKeyReconciler
	Managed              RuntimeManagedConfigurationReconciler
	AutoStop             RuntimeAutoStopController
	IDs                  IDGenerator
	Now                  func() time.Time
	ProviderPollInterval time.Duration
	ProviderPollTimeout  time.Duration
}

type RuntimeReplaceOutput struct {
	RetiredRuntimeID     string `json:"retiredRuntimeId"`
	ReplacementRuntimeID string `json:"replacementRuntimeId"`
	PrivateRoute         string `json:"privateRoute,omitempty"`
}

type runtimeReplaceWorkflow struct{ dependencies RuntimeReplaceDependencies }

func RuntimeReplaceDefinition(dependencies RuntimeReplaceDependencies) restate.ServiceDefinition {
	workflow := &runtimeReplaceWorkflow{dependencies: dependencies}
	return restate.NewWorkflow(RuntimeReplaceService).Handler(RunHandler, restate.NewWorkflowHandler(workflow.Run))
}

func (workflow *runtimeReplaceWorkflow) Run(ctx restate.WorkflowContext, input domain.RuntimeOperationDispatch) (RuntimeReplaceOutput, error) {
	if restate.Key(ctx) != input.OperationID {
		return RuntimeReplaceOutput{}, restate.TerminalErrorf("workflow key does not match Operation ID")
	}
	dependencies := workflow.dependencies
	state, err := restate.Run(ctx, func(runCtx restate.RunContext) (RuntimeOperationState, error) {
		state, err := dependencies.Actions.LoadRuntimeOperation(runCtx, input, runCtx.Request().ID, dependencies.Now())
		return state, classifyDurableError(err)
	}, restate.WithName("lock-runtime-operation"))
	if err != nil {
		return RuntimeReplaceOutput{}, err
	}
	if err := validateRuntimeOperationInput(input, domain.OperationRuntimeReplace, state); err != nil {
		return RuntimeReplaceOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeReplaceFailed, err.Error(), dependencies.Now)
	}
	promotedImage, err := restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
		image, err := dependencies.Images.CurrentPromotedImage(runCtx, state.Runtime.Region)
		return image, classifyDurableError(err)
	}, restate.WithName("resolve-replacement-image"))
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeReplaceFailed, err.Error(), dependencies.Now)
	}
	if promotedImage == "" {
		return RuntimeReplaceOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeReplaceFailed, "promoted image is required", dependencies.Now)
	}
	return fulfillRuntimeReplacement(ctx, dependencies, input, state, promotedImage)
}

func fulfillRuntimeReplacement(ctx restate.WorkflowContext, dependencies RuntimeReplaceDependencies, input domain.RuntimeOperationDispatch, state RuntimeOperationState, image string) (RuntimeReplaceOutput, error) {
	if dependencies.Actions == nil || dependencies.HostIdentity == nil || dependencies.Managed == nil {
		return RuntimeReplaceOutput{}, restate.TerminalErrorf("replacement actions, SSH host identity, and managed configuration reconcilers are required")
	}
	if dependencies.Attachments == nil {
		return RuntimeReplaceOutput{}, restate.TerminalErrorf("Runtime data-volume attachment observer is required")
	}
	oldRuntimeID := state.Runtime.ID
	oldProviderID := ""
	if state.Runtime.ProviderInstanceRef != nil {
		oldProviderID = *state.Runtime.ProviderInstanceRef
	}
	oldRequest, err := runtimeLifecycleRequest(state)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeReplaceFailed, err.Error(), dependencies.Now)
	}

	// Step 1: the domain transition removes the writable route immediately;
	// suppressing Auto-stop prevents policy work from racing replacement.
	old, err := domain.RestoreRuntime(state.Runtime)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeReplaceFailed, err.Error(), dependencies.Now)
	}
	replacing, err := old.BeginReplacement(dependencies.Now())
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeReplaceFailed, err.Error(), dependencies.Now)
	}
	state.Runtime, err = persistRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "begin-runtime-replacement", state.Runtime, replacing)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeReplaceFailed, err.Error(), dependencies.Now)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.AutoStop.SuppressAutoStop(runCtx, input.EnvironmentID, oldRuntimeID))
	}, restate.WithName("suppress-auto-stop-for-replacement")); err != nil {
		return RuntimeReplaceOutput{}, failRuntimeOperation(ctx, dependencies.Actions, input.OperationID, RuntimeReplaceFailed, err.Error(), dependencies.Now)
	}

	// Step 2: observe first so a stopped upgrade never calls StopRuntime, while
	// a repair of running compute closes usage exactly at the stopped observation.
	observed, err := observeRuntimeForReplacement(ctx, dependencies, oldRequest, "observe-runtime-before-replacement")
	if err != nil {
		return RuntimeReplaceOutput{}, err
	}
	if observed.Failure != "" {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, observed.FailureCode, observed.Failure)
	}
	providerWasRunning := observed.Runtime.State == provider.RuntimeStateRunning
	if observed.Runtime.State == provider.RuntimeStatePending {
		observed, err = waitForProviderState(ctx, dependencies.Provider, oldRequest, observed, provider.RuntimeStateRunning, provider.RuntimeStatePending, "wait-pending-runtime-before-replacement-stop", dependencies.ProviderPollInterval, dependencies.ProviderPollTimeout, dependencies.Now)
		if err != nil {
			return RuntimeReplaceOutput{}, err
		}
		if observed.Failure != "" {
			return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, observed.FailureCode, observed.Failure)
		}
	}
	if observed.Runtime.State != provider.RuntimeStateStopped && observed.Runtime.State != provider.RuntimeStateTerminated {
		if observed.Runtime.State != provider.RuntimeStateRunning && observed.Runtime.State != provider.RuntimeStateStopping {
			return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, string(provider.ErrorCodeResourceDiverged), fmt.Sprintf("Runtime provider state %q cannot be replaced", observed.Runtime.State))
		}
		if observed.Runtime.State == provider.RuntimeStateRunning {
			observed, err = restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
				runtime, stopErr := dependencies.Provider.StopRuntime(runCtx, oldRequest)
				outcome, outcomeErr := providerOutcome(runtime, stopErr)
				return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
			}, restate.WithName("stop-runtime-for-replacement"))
			if err != nil {
				return RuntimeReplaceOutput{}, err
			}
			if observed.Failure != "" {
				return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, observed.FailureCode, observed.Failure)
			}
			if err := validateProviderRuntimeIdentity(observed.Runtime, oldRequest); err != nil {
				return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, string(provider.ErrorCodeResourceDiverged), err.Error())
			}
		}
		observed, err = waitForProviderState(ctx, dependencies.Provider, oldRequest, observed, provider.RuntimeStateStopped, provider.RuntimeStateStopping, "wait-replacement-runtime-stopped", dependencies.ProviderPollInterval, dependencies.ProviderPollTimeout, dependencies.Now, provider.RuntimeStateRunning)
		if err != nil {
			return RuntimeReplaceOutput{}, err
		}
		if observed.Failure != "" {
			return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, observed.FailureCode, observed.Failure)
		}
	}
	if providerWasRunning && state.ComputeUsageIntervalID == "" {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, "running Runtime replacement requires an open Compute Usage Interval")
	}
	if err := closeComputeUsageForProviderOutcome(ctx, dependencies.Usage, state.ComputeUsageIntervalID, observed, "close-compute-usage-for-replacement", dbstore.ComputeUsageClosedByProviderReconciliation); err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}
	state.ComputeUsageIntervalID = ""

	// Step 3: persistent data ownership and health are checked only after the
	// old writer is stopped and before any retirement or new attachment.
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.DataVolumes.VerifyRuntimeDataVolume(runCtx, dataVolumeRequest(input, state)))
	}, restate.WithName("verify-replacement-data-volume")); err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}

	// Step 4: RetireRuntime owns detachment and system-volume retirement. A
	// terminal observation is the hard gate before EnsureRuntime may attach RW.
	if observed.Runtime.State != provider.RuntimeStateTerminated {
		observed, err = restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
			runtime, retireErr := dependencies.Provider.RetireRuntime(runCtx, oldRequest)
			outcome, outcomeErr := providerOutcome(runtime, retireErr)
			return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
		}, restate.WithName("retire-runtime-provider"))
		if err != nil {
			return RuntimeReplaceOutput{}, err
		}
		if observed.Failure != "" {
			return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, observed.FailureCode, observed.Failure)
		}
		if err := validateProviderRuntimeIdentity(observed.Runtime, oldRequest); err != nil {
			return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, string(provider.ErrorCodeResourceDiverged), err.Error())
		}
		observed, err = waitForProviderState(ctx, dependencies.Provider, oldRequest, observed, provider.RuntimeStateTerminated, provider.RuntimeStateStopping, "wait-runtime-retired", dependencies.ProviderPollInterval, dependencies.ProviderPollTimeout, dependencies.Now, provider.RuntimeStateStopped)
		if err != nil {
			return RuntimeReplaceOutput{}, err
		}
		if observed.Failure != "" {
			return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, observed.FailureCode, observed.Failure)
		}
	}
	if err := waitForOldDataVolumeDetachment(ctx, dependencies, oldRequest); err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}

	old, err = domain.RestoreRuntime(state.Runtime)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}
	retired, err := old.Retire(domain.RuntimeStateObservation{ProviderInstanceRef: oldProviderID, ExpectedVersion: state.Runtime.Version, ObservedAt: observed.ObservedAt})
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}

	// Step 5 and record-retention half of step 9: reserve a fresh sequence in
	// the same placement and atomically retire/switch rows without deleting history.
	replacementID, err := restate.Run(ctx, func(restate.RunContext) (string, error) {
		return dependencies.IDs.NewID(), nil
	}, restate.WithName("reserve-replacement-runtime-identity"))
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}
	reservation := domain.RuntimeReservation{
		ID: replacementID, EnvironmentID: input.EnvironmentID, Sequence: state.Runtime.Sequence + 1,
		RuntimePreset: state.Runtime.RuntimePreset, Region: state.Runtime.Region,
		AvailabilityZone: state.Runtime.AvailabilityZone, ImageVersion: image, CreatedAt: dependencies.Now(),
	}
	replacement, err := restate.Run(ctx, func(runCtx restate.RunContext) (domain.RuntimeSnapshot, error) {
		next, persistErr := dependencies.Actions.PersistRuntimeReplacement(runCtx, input.OperationID, input.OwnerUserID, state.Runtime.Version, retired.Snapshot(), reservation)
		return next, classifyDurableError(persistErr)
	}, restate.WithName("retire-and-reserve-replacement-runtime"))
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}

	// Step 6: the preceding terminal old-runtime observation is the explicit
	// safety gate. EnsureRuntime may now create compute and attach the data RW.
	ensureRequest := provider.EnsureRuntimeRequest{RuntimeSpec: provider.RuntimeSpec{
		RuntimeID: replacement.ID, EnvironmentID: replacement.EnvironmentID, Sequence: replacement.Sequence,
		Region: replacement.Region, AvailabilityZone: replacement.AvailabilityZone,
		RuntimePreset: replacement.RuntimePreset, ImageVersion: replacement.ImageVersion,
		DataVolumeProviderID: state.DataVolumeProviderID,
	}, OperationID: input.OperationID}
	ensured, err := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		runtime, ensureErr := dependencies.Provider.EnsureRuntime(runCtx, ensureRequest)
		outcome, outcomeErr := providerOutcome(runtime, ensureErr)
		return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
	}, restate.WithName("ensure-replacement-runtime"))
	if err != nil {
		return RuntimeReplaceOutput{}, err
	}
	if ensured.Failure != "" {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, ensured.FailureCode, ensured.Failure)
	}
	if ensured.Runtime.RuntimeSpec != ensureRequest.RuntimeSpec || ensured.Runtime.ProviderID == "" {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, string(provider.ErrorCodeResourceDiverged), "replacement Runtime provider identity diverged")
	}
	if ensured.Runtime.State != provider.RuntimeStatePending && ensured.Runtime.State != provider.RuntimeStateRunning {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, string(provider.ErrorCodeResourceDiverged), fmt.Sprintf("replacement Runtime provider state is %q", ensured.Runtime.State))
	}
	next, err := domain.RestoreRuntime(replacement)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}
	provisioned, err := next.Provision(ensured.Runtime.ProviderID, ensured.ObservedAt)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}
	replacement, err = persistReplacementRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "provision-replacement-runtime", replacement, provisioned)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}
	next, _ = domain.RestoreRuntime(replacement)
	starting, err := next.BeginStart(dependencies.Now())
	if err != nil {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, err.Error())
	}
	replacement, err = persistReplacementRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "start-replacement-runtime", replacement, starting)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}

	usageID, err := restate.Run(ctx, func(restate.RunContext) (string, error) { return dependencies.IDs.NewID(), nil }, restate.WithName("reserve-replacement-compute-usage-identity"))
	if err != nil {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, err.Error())
	}
	interval, err := restate.Run(ctx, func(runCtx restate.RunContext) (dbstore.ComputeUsageInterval, error) {
		value, openErr := dependencies.Usage.OpenComputeUsageInterval(runCtx, dbstore.OpenComputeUsageIntervalInput{
			ID: usageID, UserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID,
			RuntimeID: replacement.ID, StartedAt: ensured.ObservedAt,
		})
		return value, classifyDurableError(openErr)
	}, restate.WithName("open-replacement-compute-usage"))
	if err != nil {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, err.Error())
	}
	if interval.UserID != input.OwnerUserID || interval.EnvironmentID != input.EnvironmentID || interval.RuntimeID != replacement.ID {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, "Compute Usage Interval ownership diverged")
	}

	request := provider.RuntimeLifecycleRequest{RuntimeSpec: ensureRequest.RuntimeSpec, ProviderID: ensured.Runtime.ProviderID}
	running, err := waitForProviderState(ctx, dependencies.Provider, request, ensured, provider.RuntimeStateRunning, provider.RuntimeStatePending, "wait-replacement-runtime-running", dependencies.ProviderPollInterval, dependencies.ProviderPollTimeout, dependencies.Now)
	if err != nil {
		return RuntimeReplaceOutput{}, err
	}
	if running.Failure != "" {
		if closeErr := closeComputeUsageForProviderOutcome(ctx, dependencies.Usage, interval.ID, running, "close-replacement-usage-after-provider-failure", dbstore.ComputeUsageClosedByProviderReconciliation); closeErr != nil {
			return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, closeErr.Error())
		}
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, running.FailureCode, running.Failure)
	}

	// Steps 7-8: the current boot must prove the durable data mount before host
	// identity, keys, managed configuration, readiness, and proxy re-admission.
	guestRequest := RuntimeGuestReadinessRequest{OwnerUserID: input.OwnerUserID, EnvironmentID: input.EnvironmentID, RuntimeID: replacement.ID, ProviderID: request.ProviderID, PrivateIPv4: running.Runtime.PrivateIPv4}
	ready, err := restate.Run(ctx, func(runCtx restate.RunContext) (RuntimeGuestReadiness, error) {
		value, readyErr := dependencies.Guest.WaitForRuntimeReady(runCtx, guestRequest)
		return value, classifyDurableError(readyErr)
	}, restate.WithName("wait-for-replacement-guest-readiness"))
	if err != nil {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, GuestNotReady, err.Error())
	}
	if !ready.DataMounted || ready.BootID == "" || ready.PrivateIPv4 == "" || ready.PrivateIPv4 != running.Runtime.PrivateIPv4 {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, GuestNotReady, "current boot and mounted data are required")
	}
	guestRequest.PrivateIPv4 = ready.PrivateIPv4
	reconciliations := []struct {
		name string
		run  func(restate.RunContext) error
	}{
		{name: "restore-runtime-ssh-host-identity", run: func(runCtx restate.RunContext) error {
			return dependencies.HostIdentity.RestoreRuntimeSSHHostIdentity(runCtx, guestRequest)
		}},
		{name: "reconcile-runtime-ssh-keys", run: func(runCtx restate.RunContext) error {
			return dependencies.SSHKeys.ReconcileRuntimeSSHKeys(runCtx, guestRequest)
		}},
		{name: "reconcile-runtime-managed-configuration", run: func(runCtx restate.RunContext) error {
			return dependencies.Managed.ReconcileRuntimeManagedConfiguration(runCtx, guestRequest)
		}},
	}
	for _, reconciliation := range reconciliations {
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error { return classifyDurableError(reconciliation.run(runCtx)) }, restate.WithName(reconciliation.name)); err != nil {
			return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, err.Error())
		}
	}
	next, _ = domain.RestoreRuntime(replacement)
	markedReady, err := next.MarkReady(domain.RuntimeReadinessObservation{ProviderInstanceRef: request.ProviderID, BootID: ready.BootID, PrivateAddress: ready.PrivateIPv4, ExpectedVersion: replacement.Version, ObservedAt: dependencies.Now()})
	if err != nil {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, err.Error())
	}
	replacement, err = persistReplacementRuntimeTransition(ctx, dependencies.Actions, input.OperationID, "mark-replacement-runtime-ready", replacement, markedReady)
	if err != nil {
		return RuntimeReplaceOutput{}, failRuntimeReplacement(ctx, dependencies, input.OperationID, RuntimeReplaceFailed, err.Error())
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.AutoStop.ResumeAutoStop(runCtx, input.EnvironmentID, replacement.ID))
	}, restate.WithName("resume-auto-stop-after-replacement")); err != nil {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, err.Error())
	}
	if err := completeRuntimeOperation(ctx, dependencies.Actions, input.OperationID, dependencies.Now); err != nil {
		return RuntimeReplaceOutput{}, failReplacementRuntimeAndOperation(ctx, dependencies, input.OperationID, replacement, RuntimeReplaceFailed, err.Error())
	}
	return RuntimeReplaceOutput{RetiredRuntimeID: oldRuntimeID, ReplacementRuntimeID: replacement.ID, PrivateRoute: ready.PrivateIPv4}, nil
}

func observeRuntimeForReplacement(ctx restate.WorkflowContext, dependencies RuntimeReplaceDependencies, request provider.RuntimeLifecycleRequest, name string) (runtimeProviderOutcome, error) {
	observed, err := restate.Run(ctx, func(runCtx restate.RunContext) (runtimeProviderOutcome, error) {
		runtime, observeErr := dependencies.Provider.ObserveRuntime(runCtx, request)
		outcome, outcomeErr := providerOutcome(runtime, observeErr)
		return timestampProviderOutcome(outcome, outcomeErr, dependencies.Now())
	}, restate.WithName(name))
	if err == nil && observed.Failure == "" {
		if identityErr := validateProviderRuntimeIdentity(observed.Runtime, request); identityErr != nil {
			return providerDivergenceOutcome(identityErr), nil
		}
	}
	return observed, err
}

type runtimeAttachmentOutcome struct {
	Attachment       provider.RuntimeDataVolumeAttachment `json:"attachment"`
	ObservedAt       time.Time                            `json:"observedAt"`
	RetryableFailure bool                                 `json:"retryableFailure,omitempty"`
	Failure          string                               `json:"failure,omitempty"`
}

func waitForOldDataVolumeDetachment(ctx restate.WorkflowContext, dependencies RuntimeReplaceDependencies, request provider.RuntimeLifecycleRequest) error {
	pollInterval := dependencies.ProviderPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultProviderPollInterval
	}
	pollTimeout := dependencies.ProviderPollTimeout
	if pollTimeout <= 0 {
		pollTimeout = defaultProviderPollTimeout
	}
	read := func(runCtx restate.RunContext, _ time.Time) (durableDeadlinePollRead[runtimeAttachmentOutcome], error) {
		attachment, err := dependencies.Attachments.ObserveRuntimeDataVolumeAttachment(runCtx, request)
		outcome := runtimeAttachmentOutcome{Attachment: attachment}
		if err != nil {
			var classified interface{ Transient() bool }
			if errors.As(err, &classified) && classified.Transient() {
				outcome.RetryableFailure = true
			} else {
				outcome.Failure = err.Error()
			}
		}
		return durableDeadlinePollRead[runtimeAttachmentOutcome]{Value: outcome, UseValue: true, RetryableFailure: outcome.RetryableFailure}, nil
	}
	poll, err := durableDeadlinePoll(ctx, (*durableDeadlinePollValue[runtimeAttachmentOutcome])(nil), durableDeadlinePollConfig{
		timeout: pollTimeout, initialDelay: pollInterval,
		maxDelay: 30 * time.Second, stepPrefix: "wait-retired-data-volume-detached",
		readStepPrefix: "wait-retired-data-volume-detached-observe", now: dependencies.Now,
	}, read, func(outcome runtimeAttachmentOutcome, _ time.Time) (runtimeAttachmentOutcome, bool) {
		if outcome.RetryableFailure {
			return outcome, false
		}
		if outcome.Failure != "" {
			return outcome, true
		}
		attachment := outcome.Attachment
		if attachment.DataVolumeProviderID != request.DataVolumeProviderID {
			outcome.Failure = "Data Volume attachment observation identity diverged"
			return outcome, true
		}
		if !attachment.Attached {
			return outcome, true
		}
		if attachment.RuntimeProviderID != request.ProviderID || !attachment.ReadWrite {
			outcome.Failure = "old Data Volume attachment ownership diverged"
			return outcome, true
		}
		return outcome, false
	}, func(outcome runtimeAttachmentOutcome, checkedAt time.Time) runtimeAttachmentOutcome {
		outcome.ObservedAt = checkedAt
		return outcome
	})
	if err != nil {
		return err
	}
	if poll.timedOut {
		return fmt.Errorf("Data Volume remained attached after old Runtime retirement")
	}
	if poll.value.Failure != "" {
		return errors.New(poll.value.Failure)
	}
	return nil
}

func persistReplacementRuntimeTransition(ctx restate.WorkflowContext, actions RuntimeReplaceActions, operationID, name string, before domain.RuntimeSnapshot, after domain.Runtime) (domain.RuntimeSnapshot, error) {
	next := after.Snapshot()
	return restate.Run(ctx, func(runCtx restate.RunContext) (domain.RuntimeSnapshot, error) {
		if err := actions.PersistReplacementRuntimeTransition(runCtx, operationID, before.Version, next); err != nil {
			return domain.RuntimeSnapshot{}, classifyDurableError(err)
		}
		return next, nil
	}, restate.WithName(name))
}

func failRuntimeReplacement(ctx restate.WorkflowContext, dependencies RuntimeReplaceDependencies, operationID, code, message string) error {
	return failRuntimeOperation(ctx, dependencies.Actions, operationID, code, message, dependencies.Now)
}

func failReplacementRuntimeAndOperation(ctx restate.WorkflowContext, dependencies RuntimeReplaceDependencies, operationID string, current domain.RuntimeSnapshot, code, message string) error {
	runtime, err := domain.RestoreRuntime(current)
	if err == nil && current.ProviderInstanceRef != nil && current.Status != domain.RuntimeError {
		failed, markErr := runtime.MarkError(domain.RuntimeStateObservation{ProviderInstanceRef: *current.ProviderInstanceRef, ExpectedVersion: current.Version, ObservedAt: dependencies.Now()})
		if markErr == nil {
			if _, persistErr := persistReplacementRuntimeTransition(ctx, dependencies.Actions, operationID, "mark-replacement-runtime-error", current, failed); persistErr != nil {
				return persistErr
			}
		}
	}
	return failRuntimeReplacement(ctx, dependencies, operationID, code, message)
}
