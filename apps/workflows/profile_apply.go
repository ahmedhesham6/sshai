package workflows

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
	restate "github.com/restatedev/sdk-go"
)

const (
	ProfileApplyService = "ProfileApply"
	ProfileApplyFailed  = "PROFILE_APPLY_FAILED"
)

var ErrGuestUnavailable = errors.New("guest control transport is unavailable")

type ProfileApplyOperationInput struct {
	ProfileVersionID    string   `json:"profileVersionId"`
	ApprovedReviewItems []string `json:"approvedReviewItems,omitempty"`
}

type ProfileApplyState struct {
	OwnerUserID      string
	Runtime          domain.RuntimeSnapshot
	Version          ProfileVersionData
	PreviousLock     *domain.CapsuleLockSnapshot
	Materializations []profile.InstalledMaterialization
	UpgradePolicy    domain.UpgradePolicy
	Request          ProfileApplyOperationInput
}

type ProfileApplyActions interface {
	LoadProfileApplyOperation(context.Context, domain.RuntimeOperationDispatch, string, time.Time) (ProfileApplyState, error)
	CompleteProfileApply(context.Context, string, *string, EnvironmentCapsuleState, time.Time) error
	RecordProfileApplyFailure(context.Context, string, string, string, time.Time) error
}

type ProfileApplyDependencies struct {
	Actions            ProfileApplyActions
	Resolver           CapsuleResolver
	CapsuleApplication EnvironmentCapsuleApplier
	IDs                IDGenerator
	Now                func() time.Time
	RetryInterval      time.Duration
	Timeout            time.Duration
}

type ProfileApplyOutput struct {
	ProfileVersionID string `json:"profileVersionId"`
	CapsuleLockID    string `json:"capsuleLockId"`
}

type profileApplyWorkflow struct{ dependencies ProfileApplyDependencies }

func ProfileApplyDefinition(dependencies ProfileApplyDependencies) restate.ServiceDefinition {
	return restate.NewWorkflow(ProfileApplyService).Handler(RunHandler, restate.NewWorkflowHandler((&profileApplyWorkflow{dependencies: dependencies}).Run))
}

func (workflow *profileApplyWorkflow) Run(ctx restate.WorkflowContext, input domain.RuntimeOperationDispatch) (ProfileApplyOutput, error) {
	if restate.Key(ctx) != input.OperationID {
		return ProfileApplyOutput{}, restate.TerminalErrorf("workflow key does not match Operation ID")
	}
	dependencies := workflow.dependencies
	if dependencies.Actions == nil || dependencies.Resolver == nil || dependencies.CapsuleApplication == nil || dependencies.IDs == nil || dependencies.Now == nil {
		return ProfileApplyOutput{}, restate.TerminalErrorf("Profile apply workflow dependencies are incomplete")
	}
	state, err := restate.Run(ctx, func(runCtx restate.RunContext) (ProfileApplyState, error) {
		return dependencies.Actions.LoadProfileApplyOperation(runCtx, input, runCtx.Request().ID, dependencies.Now().UTC())
	}, restate.WithName("record-invocation-and-load-state"))
	if err != nil {
		return ProfileApplyOutput{}, err
	}
	if err := validateProfileApplyState(input, state); err != nil {
		return ProfileApplyOutput{}, failProfileApply(ctx, dependencies, input.OperationID, ProfileApplyFailed, err)
	}
	lastApproved := make(map[string]string)
	projectDigest := (*string)(nil)
	previousLockID := (*string)(nil)
	if state.PreviousLock != nil {
		lockID := state.PreviousLock.ID
		previousLockID = &lockID
		project := state.PreviousLock.ProjectCapsuleDigest
		projectDigest = &project
		for _, capsule := range state.PreviousLock.Capsules {
			lastApproved[capsule.Ref] = capsule.Digest
		}
	}
	resolved, err := restate.Run(ctx, func(runCtx restate.RunContext) (resolvedCapsuleSet, error) {
		resolver := profileResolveWorkflow{resolver: dependencies.Resolver}
		value, resolveErr := resolver.resolveCapsules(runCtx, state.OwnerUserID, state.Version, projectDigest, lastApproved)
		return value, classifyDurableError(resolveErr)
	}, restate.WithName("resolve-profile-version"))
	if err != nil {
		return ProfileApplyOutput{}, failProfileApply(ctx, dependencies, input.OperationID, "PROFILE_INCOMPATIBLE", err)
	}
	if missing := missingReviewApprovals(resolved, state.Request.ApprovedReviewItems); len(missing) > 0 {
		return ProfileApplyOutput{}, failProfileApply(ctx, dependencies, input.OperationID, "PROFILE_REVIEW_REQUIRED", fmt.Errorf("review approval is required for %s", strings.Join(missing, ", ")))
	}
	lock, err := restate.Run(ctx, func(restate.RunContext) (domain.CapsuleLockSnapshot, error) {
		created, createErr := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
			ID: dependencies.IDs.NewID(), EnvironmentID: input.EnvironmentID, ProfileVersionID: state.Request.ProfileVersionID,
			ProjectCapsuleDigest: resolved.ProjectDigest, Capsules: resolved.LockedCapsules,
			ResolvedComponents: resolved.Components, CreatedAt: dependencies.Now().UTC(),
		})
		if createErr != nil {
			return domain.CapsuleLockSnapshot{}, createErr
		}
		return created.Snapshot(), nil
	}, restate.WithName("create-candidate-lock"))
	if err != nil {
		return ProfileApplyOutput{}, failProfileApply(ctx, dependencies, input.OperationID, "PROFILE_INCOMPATIBLE", err)
	}
	stateForGuest := EnvironmentCapsuleState{
		CapsuleLock: lock, UpgradePolicy: state.UpgradePolicy, Materializations: state.Materializations,
		Approvals: approvalMarkers(lock, state.Request.ApprovedReviewItems),
	}
	guestRequest := EnvironmentCreateGuestRequest{OperationID: input.OperationID, RuntimeGuestReadinessRequest: RuntimeGuestReadinessRequest{
		OwnerUserID: state.OwnerUserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID,
		ProviderID: *state.Runtime.ProviderInstanceRef, PrivateIPv4: *state.Runtime.PrivateAddress,
	}}
	results, materializeErr := materializeProfileApply(ctx, dependencies, EnvironmentCapsuleMaterializationRequest{Guest: guestRequest, State: stateForGuest})
	if materializeErr != nil {
		code := "PROFILE_CONFLICT"
		if errors.Is(materializeErr, ErrGuestUnavailable) {
			code = GuestNotReady
		}
		return ProfileApplyOutput{}, failProfileApply(ctx, dependencies, input.OperationID, code, materializeErr)
	}
	if err := validateEnvironmentMaterializationResults(lock, results); err != nil {
		return ProfileApplyOutput{}, failProfileApply(ctx, dependencies, input.OperationID, "PROFILE_CONFLICT", err)
	}
	stateForGuest.ApplyResults = results
	stateForGuest.Materializations = InstalledMaterializationsFromApplyResults(results)
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.CompleteProfileApply(runCtx, input.OperationID, previousLockID, stateForGuest, dependencies.Now().UTC()))
	}, restate.WithName("pin-validated-capsule-state-and-complete")); err != nil {
		return ProfileApplyOutput{}, failProfileApply(ctx, dependencies, input.OperationID, ProfileApplyFailed, err)
	}
	return ProfileApplyOutput{ProfileVersionID: state.Request.ProfileVersionID, CapsuleLockID: lock.ID}, nil
}

type profileApplyMaterializationOutcome struct {
	Results          []profile.ProfileMaterializationResult
	Failure          string
	GuestUnavailable bool
}

func materializeProfileApply(ctx restate.WorkflowContext, dependencies ProfileApplyDependencies, request EnvironmentCapsuleMaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	interval, timeout := dependencies.RetryInterval, dependencies.Timeout
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	poll, err := durableDeadlinePoll(ctx, nil, durableDeadlinePollConfig{
		timeout: timeout, initialDelay: interval, maxDelay: 30 * time.Second,
		stepPrefix: "materialize-profile", readStepPrefix: "apply-capsule-lock", now: dependencies.Now,
	}, func(runCtx restate.RunContext, _ time.Time) (durableDeadlinePollRead[profileApplyMaterializationOutcome], error) {
		results, applyErr := dependencies.CapsuleApplication.EnsureEnvironmentCapsuleMaterialized(runCtx, request)
		if applyErr == nil {
			return durableDeadlinePollRead[profileApplyMaterializationOutcome]{Value: profileApplyMaterializationOutcome{Results: results}, UseValue: true}, nil
		}
		if isTransientError(applyErr) {
			return durableDeadlinePollRead[profileApplyMaterializationOutcome]{RetryableFailure: true}, nil
		}
		return durableDeadlinePollRead[profileApplyMaterializationOutcome]{Value: profileApplyMaterializationOutcome{
			Failure: applyErr.Error(), GuestUnavailable: errors.Is(applyErr, ErrGuestUnavailable),
		}, UseValue: true}, nil
	}, func(value profileApplyMaterializationOutcome, _ time.Time) (profileApplyMaterializationOutcome, bool) {
		return value, true
	}, nil)
	if err != nil {
		return nil, err
	}
	if poll.timedOut {
		return nil, fmt.Errorf("%w: Capsule Lock materialization timed out", ErrGuestUnavailable)
	}
	if poll.value.Failure != "" {
		if poll.value.GuestUnavailable {
			return nil, fmt.Errorf("%w: %s", ErrGuestUnavailable, poll.value.Failure)
		}
		return nil, errors.New(poll.value.Failure)
	}
	return poll.value.Results, nil
}

func validateProfileApplyState(input domain.RuntimeOperationDispatch, state ProfileApplyState) error {
	if err := validateRuntimeOperationInput(input, domain.OperationProfileApply, RuntimeOperationState{OwnerUserID: state.OwnerUserID, Runtime: state.Runtime}); err != nil {
		return err
	}
	if state.Runtime.Status != domain.RuntimeReady || state.Runtime.ProviderInstanceRef == nil || state.Runtime.PrivateAddress == nil || state.Runtime.BootID == nil {
		return errors.New("Profile application requires the current Runtime to be ready")
	}
	if strings.TrimSpace(state.Request.ProfileVersionID) == "" || state.Version.ID != state.Request.ProfileVersionID {
		return errors.New("persisted Profile Version input is invalid")
	}
	return nil
}

func missingReviewApprovals(resolved resolvedCapsuleSet, approved []string) []string {
	granted := make(map[string]struct{}, len(approved))
	for _, item := range approved {
		granted[item] = struct{}{}
	}
	var missing []string
	for ref := range resolved.DiffSinceLastApproval {
		if _, ok := granted[ref]; !ok {
			missing = append(missing, ref)
		}
	}
	sort.Strings(missing)
	return missing
}

func approvalMarkers(lock domain.CapsuleLockSnapshot, approved []string) map[string]profile.ApprovalMarker {
	markers := make(map[string]profile.ApprovalMarker)
	for _, componentID := range approved {
		component, ok := lock.ResolvedComponents[componentID]
		if ok {
			markers[componentID] = profile.ApprovalMarker{ComponentID: componentID, ComponentDigest: component.ComponentDigest, LockID: lock.ID, LockDigest: lock.Digest}
		}
	}
	return markers
}

func failProfileApply(ctx restate.WorkflowContext, dependencies ProfileApplyDependencies, operationID, code string, cause error) error {
	if code == "" {
		code = ProfileApplyFailed
	}
	message := code
	if cause != nil {
		message = cause.Error()
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return classifyDurableError(dependencies.Actions.RecordProfileApplyFailure(runCtx, operationID, code, message, dependencies.Now().UTC()))
	}, restate.WithName("record-profile-apply-failure")); err != nil {
		return err
	}
	return restate.TerminalErrorf("%s: %s", code, message)
}
