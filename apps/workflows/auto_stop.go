package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

const (
	AutoStopService                    = "EnvironmentAutoStop"
	AutoStopObserveHandler             = "Observe"
	AutoStopExpireHandler              = "Expire"
	AutoStopSuppressHandler            = "Suppress"
	AutoStopResumeHandler              = "Resume"
	AutoStopRefreshHandler             = "Refresh"
	autoStopStateKey                   = "coordination"
	autoStopStaleThreshold             = 5 * time.Minute
	autoStopSnapshotPollTimeout        = 90 * time.Second
	autoStopSnapshotPollInitialBackoff = 250 * time.Millisecond
	autoStopSnapshotPollMaxBackoff     = 5 * time.Second
)

type AutoStopRefreshRequest struct {
	EnvironmentID         string
	RuntimeID             string
	AfterSnapshotSequence uint64
	FreshAfter            time.Time
}

type AutoStopSnapshotSource interface {
	ReadAutoStopState(context.Context, AutoStopRefreshRequest) (AutoStopObservation, error)
	ReadLatestSnapshot(context.Context, AutoStopRefreshRequest) (*domain.AutoStopActivitySnapshot, error)
}

type AutoStopRuntimeRequest struct {
	RuntimeID string `json:"runtimeId"`
}

type AutoStopRefreshSignal struct{}

type RuntimeAutoStopController interface {
	SuppressAutoStop(context.Context, string, string) error
	ResumeAutoStop(context.Context, string, string) error
}

type RuntimeStopDispatcher interface {
	DispatchRuntimeStop(context.Context, RuntimeStopRequest) error
}

type autoStopObject struct {
	source     AutoStopSnapshotSource
	dispatcher RuntimeStopDispatcher
}

type autoStopSnapshotRead struct {
	Observation          AutoStopObservation
	ReferenceUnavailable bool
}

type autoStopSnapshotPolling struct {
	timeout        time.Duration
	initialBackoff time.Duration
	maxBackoff     time.Duration
	now            func() time.Time
}

type autoStopDispatchDisposition string

const (
	autoStopDispatchConflict             autoStopDispatchDisposition = "idempotency_conflict"
	autoStopDispatchReferenceUnavailable autoStopDispatchDisposition = "reference_unavailable"
)

func AutoStopDefinition(source AutoStopSnapshotSource, dispatcher RuntimeStopDispatcher) restate.ServiceDefinition {
	object := &autoStopObject{source: source, dispatcher: dispatcher}
	return restate.NewObject(AutoStopService).
		Handler(AutoStopObserveHandler, restate.NewObjectHandler(object.Observe)).
		Handler(AutoStopExpireHandler, restate.NewObjectHandler(object.Expire)).
		Handler(AutoStopSuppressHandler, restate.NewObjectHandler(object.Suppress)).
		Handler(AutoStopResumeHandler, restate.NewObjectHandler(object.Resume)).
		Handler(AutoStopRefreshHandler, restate.NewObjectHandler(object.Refresh))
}

func (object *autoStopObject) Suppress(ctx restate.ObjectContext, input AutoStopRuntimeRequest) (AutoStopTransition, error) {
	state, err := object.coordinationState(ctx)
	if err != nil {
		return AutoStopTransition{}, err
	}
	transition, err := (AutoStopCoordinator{}).Suppress(state, input.RuntimeID)
	if err != nil {
		return AutoStopTransition{}, restate.TerminalErrorf("invalid Auto-stop suppression: %v", err)
	}
	restate.Set(ctx, autoStopStateKey, transition.State)
	return transition, nil
}

func (object *autoStopObject) Resume(ctx restate.ObjectContext, input AutoStopRuntimeRequest) (AutoStopTransition, error) {
	state, err := object.coordinationState(ctx)
	if err != nil {
		return AutoStopTransition{}, err
	}
	transition, err := (AutoStopCoordinator{}).Resume(state, input.RuntimeID)
	if err != nil {
		return AutoStopTransition{}, restate.TerminalErrorf("invalid Auto-stop resume: %v", err)
	}
	restate.Set(ctx, autoStopStateKey, transition.State)
	return transition, nil
}

func (*autoStopObject) coordinationState(ctx restate.ObjectContext) (AutoStopCoordinationState, error) {
	state, err := restate.Get[AutoStopCoordinationState](ctx, autoStopStateKey)
	if err != nil {
		return AutoStopCoordinationState{}, err
	}
	if state.EnvironmentID == "" {
		state.EnvironmentID = restate.Key(ctx)
	}
	return state, nil
}

func (object *autoStopObject) Observe(ctx restate.ObjectContext, input AutoStopObservation) (AutoStopTransition, error) {
	state, getErr := restate.Get[AutoStopCoordinationState](ctx, autoStopStateKey)
	if getErr != nil {
		return AutoStopTransition{}, getErr
	}
	if state.EnvironmentID == "" {
		state.EnvironmentID = restate.Key(ctx)
	}
	processedAt, processedAtErr := restate.Run(ctx, func(restate.RunContext) (time.Time, error) {
		return time.Now().UTC(), nil
	}, restate.WithName("record-auto-stop-processing-time"))
	if processedAtErr != nil {
		return AutoStopTransition{}, processedAtErr
	}
	input.ProcessedAt = processedAt
	transition, err := (AutoStopCoordinator{}).Observe(state, input)
	if err != nil {
		return AutoStopTransition{}, restate.TerminalErrorf("invalid Auto-stop observation: %v", err)
	}
	restate.Set(ctx, autoStopStateKey, transition.State)
	object.schedule(ctx, transition.Timer)
	return transition, nil
}

// Refresh re-reads the current policy and latest stored Activity Snapshot.
// Policy-update signals are idempotent Restate sends, so replaying one can
// only reproduce the same coordinator transition and timer generation.
func (object *autoStopObject) Refresh(ctx restate.ObjectContext, _ AutoStopRefreshSignal) (AutoStopTransition, error) {
	state, err := object.coordinationState(ctx)
	if err != nil || state.RuntimeID == "" {
		return AutoStopTransition{State: state}, err
	}
	processedAt, err := restate.Run(ctx, func(restate.RunContext) (time.Time, error) {
		return time.Now().UTC(), nil
	}, restate.WithName("record-auto-stop-refresh-time"))
	if err != nil {
		return AutoStopTransition{}, err
	}
	request := AutoStopRefreshRequest{
		EnvironmentID: state.EnvironmentID, RuntimeID: state.RuntimeID,
		FreshAfter: processedAt.Add(-autoStopStaleThreshold),
	}
	read, err := refreshAutoStopSnapshot(ctx, object.source, request, autoStopSnapshotPolling{}, "refresh-auto-stop-policy")
	if err != nil {
		return AutoStopTransition{}, err
	}
	if read.ReferenceUnavailable {
		transition := abandonAutoStopRuntime(state)
		restate.Set(ctx, autoStopStateKey, transition.State)
		return transition, nil
	}
	observation := read.Observation
	observation.FreshAfter = processedAt.Add(-autoStopStaleThreshold)
	observation.ProcessedAt = processedAt
	transition, err := (AutoStopCoordinator{}).Observe(state, observation)
	if err != nil {
		return AutoStopTransition{}, restate.TerminalErrorf("invalid Auto-stop refresh: %v", err)
	}
	restate.Set(ctx, autoStopStateKey, transition.State)
	object.schedule(ctx, transition.Timer)
	return transition, nil
}

func (object *autoStopObject) Expire(ctx restate.ObjectContext, timer AutoStopTimer) (AutoStopTransition, error) {
	state, getErr := restate.Get[AutoStopCoordinationState](ctx, autoStopStateKey)
	if getErr != nil {
		return AutoStopTransition{}, getErr
	}
	if !currentAutoStopTimer(state, timer) {
		return AutoStopTransition{State: state}, nil
	}
	freshAfter, timeErr := restate.Run(ctx, func(restate.RunContext) (time.Time, error) {
		return time.Now().UTC(), nil
	}, restate.WithName("record-auto-stop-expiry-refresh-time"))
	if timeErr != nil {
		return AutoStopTransition{}, timeErr
	}
	request := AutoStopRefreshRequest{
		EnvironmentID: state.EnvironmentID, RuntimeID: state.RuntimeID,
		AfterSnapshotSequence: state.LastSnapshotSequence, FreshAfter: freshAfter,
	}
	read, err := refreshAutoStopSnapshot(ctx, object.source, request, autoStopSnapshotPolling{}, "refresh-activity-snapshot")
	if err != nil {
		return AutoStopTransition{}, err
	}
	if read.ReferenceUnavailable {
		transition := abandonAutoStopRuntime(state)
		restate.Set(ctx, autoStopStateKey, transition.State)
		return transition, nil
	}
	observation := read.Observation
	processedAt, timeErr := restate.Run(ctx, func(restate.RunContext) (time.Time, error) {
		return time.Now().UTC(), nil
	}, restate.WithName("record-auto-stop-expiry-processing-time"))
	if timeErr != nil {
		return AutoStopTransition{}, timeErr
	}
	observation.ProcessedAt = processedAt
	transition, evaluationErr := (AutoStopCoordinator{}).Expire(state, AutoStopExpiry{
		RuntimeID: timer.RuntimeID, Generation: timer.Generation, Observation: observation,
	})
	if evaluationErr != nil {
		return AutoStopTransition{}, restate.TerminalErrorf("invalid Auto-stop expiry: %v", evaluationErr)
	}
	if transition.Stop != nil {
		request := *transition.Stop
		disposition, err := restate.Run(ctx, func(runCtx restate.RunContext) (autoStopDispatchDisposition, error) {
			err := object.dispatcher.DispatchRuntimeStop(runCtx, request)
			return classifyAutoStopDispatchError(err)
		}, restate.WithName("dispatch-runtime-stop"))
		if err != nil {
			return AutoStopTransition{}, err
		}
		if disposition != "" {
			message := "automatic Runtime stop idempotency conflict ended dispatch cycle"
			if disposition == autoStopDispatchReferenceUnavailable {
				message = "automatic Runtime stop target disappeared; dispatch cycle ended"
			}
			slog.WarnContext(ctx, message,
				"environment_id", request.EnvironmentID, "runtime_id", request.RuntimeID,
				"timer_generation", transition.State.TimerGeneration, "idempotency_key", request.IdempotencyKey)
			transition.Stop = nil
			transition.Cancelled = true
			transition.State.TimerPending = false
			transition.State.DispatchedGeneration = 0
			transition.State.GraceStartedAt = time.Time{}
			transition.State.GraceStartSnapshot = nil
		}
	}
	restate.Set(ctx, autoStopStateKey, transition.State)
	object.schedule(ctx, transition.Timer)
	return transition, nil
}

func classifyAutoStopDispatchError(err error) (autoStopDispatchDisposition, error) {
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, db.ErrIdempotencyConflict):
		return autoStopDispatchConflict, nil
	case errors.Is(err, db.ErrReferenceNotOwned):
		return autoStopDispatchReferenceUnavailable, nil
	default:
		return "", classifyDurableError(err)
	}
}

func refreshAutoStopSnapshot(ctx restate.Context, source AutoStopSnapshotSource, request AutoStopRefreshRequest, polling autoStopSnapshotPolling, stepPrefix string) (autoStopSnapshotRead, error) {
	if source == nil || request.EnvironmentID == "" || request.RuntimeID == "" {
		return autoStopSnapshotRead{}, restate.TerminalErrorf("refresh Auto-stop Snapshot: source, Environment, and Runtime are required")
	}
	if polling.timeout <= 0 {
		polling.timeout = autoStopSnapshotPollTimeout
	}
	if polling.initialBackoff <= 0 {
		polling.initialBackoff = autoStopSnapshotPollInitialBackoff
	}
	if polling.maxBackoff <= 0 {
		polling.maxBackoff = autoStopSnapshotPollMaxBackoff
	}
	if polling.now == nil {
		polling.now = time.Now
	}
	readState := func(name string) (autoStopSnapshotRead, error) {
		return restate.Run(ctx, func(runCtx restate.RunContext) (autoStopSnapshotRead, error) {
			observation, err := source.ReadAutoStopState(runCtx, request)
			if errors.Is(err, db.ErrReferenceNotOwned) {
				return autoStopSnapshotRead{ReferenceUnavailable: true}, nil
			}
			return autoStopSnapshotRead{Observation: observation}, classifyDurableError(err)
		}, restate.WithName(name))
	}
	initial, err := readState(stepPrefix + "-state-before")
	if err != nil || initial.ReferenceUnavailable {
		return initial, err
	}

	poll, err := durableDeadlinePoll[*domain.AutoStopActivitySnapshot](ctx, nil, durableDeadlinePollConfig{
		timeout: polling.timeout, initialDelay: polling.initialBackoff, maxDelay: polling.maxBackoff,
		stepPrefix: stepPrefix, readStepPrefix: stepPrefix + "-read-snapshot", now: polling.now,
	}, func(runCtx restate.RunContext, pollStartedAt time.Time) (durableDeadlinePollRead[*domain.AutoStopActivitySnapshot], error) {
		readRequest := request
		if readRequest.FreshAfter.IsZero() {
			readRequest.FreshAfter = pollStartedAt
		}
		snapshot, readErr := source.ReadLatestSnapshot(runCtx, readRequest)
		if readErr == nil {
			return durableDeadlinePollRead[*domain.AutoStopActivitySnapshot]{Value: snapshot, UseValue: true}, nil
		}
		classified := classifyDurableError(readErr)
		if restate.IsTerminalError(classified) {
			return durableDeadlinePollRead[*domain.AutoStopActivitySnapshot]{}, classified
		}
		return durableDeadlinePollRead[*domain.AutoStopActivitySnapshot]{RetryableFailure: true}, nil
	}, func(snapshot *domain.AutoStopActivitySnapshot, pollStartedAt time.Time) (*domain.AutoStopActivitySnapshot, bool) {
		effectiveRequest := request
		if effectiveRequest.FreshAfter.IsZero() {
			effectiveRequest.FreshAfter = pollStartedAt
		}
		return snapshot, autoStopSnapshotSatisfies(snapshot, effectiveRequest)
	}, nil)
	if err != nil {
		return autoStopSnapshotRead{}, err
	}
	effectiveRequest := request
	if effectiveRequest.FreshAfter.IsZero() {
		effectiveRequest.FreshAfter = poll.startedAt
	}

	final, err := readState(stepPrefix + "-state-after")
	if err != nil || final.ReferenceUnavailable {
		return final, err
	}
	final.Observation.Snapshot = poll.value
	final.Observation.FreshAfter = effectiveRequest.FreshAfter
	return final, nil
}

func autoStopSnapshotSatisfies(snapshot *domain.AutoStopActivitySnapshot, request AutoStopRefreshRequest) bool {
	return snapshot != nil &&
		(request.AfterSnapshotSequence == 0 || snapshot.Sequence > request.AfterSnapshotSequence) &&
		(request.FreshAfter.IsZero() || !snapshot.ObservedAt.Before(request.FreshAfter))
}

func (object *autoStopObject) schedule(ctx restate.ObjectContext, timer *AutoStopTimer) {
	if timer == nil {
		return
	}
	restate.ObjectSend(ctx, AutoStopService, restate.Key(ctx), AutoStopExpireHandler).
		Send(*timer, restate.WithDelay(timer.Delay), restate.WithIdempotencyKey(
			fmt.Sprintf("auto-stop-timer:%s:%s:%d", restate.Key(ctx), timer.RuntimeID, timer.Generation),
		))
}

func currentAutoStopTimer(state AutoStopCoordinationState, timer AutoStopTimer) bool {
	return state.TimerPending && state.RuntimeID == timer.RuntimeID && state.TimerGeneration == timer.Generation
}

func abandonAutoStopRuntime(state AutoStopCoordinationState) AutoStopTransition {
	transition := AutoStopTransition{State: state, Cancelled: state.TimerPending}
	if state.TimerPending {
		transition.State.TimerGeneration++
	}
	transition.State.RuntimeID = ""
	transition.State.PolicyID = ""
	transition.State.PolicyMode = ""
	transition.State.PolicyGraceSeconds = 0
	transition.State.PolicyGeneration = 0
	transition.State.TimerPending = false
	transition.State.LastSnapshotSequence = 0
	transition.State.DispatchedGeneration = 0
	transition.State.SuppressedRuntimeID = ""
	transition.State.GraceStartedAt = time.Time{}
	transition.State.GraceStartSnapshot = nil
	return transition
}

func (client *Client) SendAutoStopObservation(ctx context.Context, environmentID string, input AutoStopObservation, idempotencyKey string) error {
	_, err := ingress.ObjectSend[AutoStopObservation](
		client.ingress, AutoStopService, environmentID, AutoStopObserveHandler,
	).Send(ctx, input, restate.WithIdempotencyKey(idempotencyKey))
	return err
}

func (client *Client) SuppressAutoStop(ctx context.Context, environmentID, runtimeID string) error {
	_, err := ingress.Object[AutoStopRuntimeRequest, AutoStopTransition](
		client.ingress, AutoStopService, environmentID, AutoStopSuppressHandler,
	).Request(ctx, AutoStopRuntimeRequest{RuntimeID: runtimeID})
	return err
}

func (client *Client) ResumeAutoStop(ctx context.Context, environmentID, runtimeID string) error {
	_, err := ingress.Object[AutoStopRuntimeRequest, AutoStopTransition](
		client.ingress, AutoStopService, environmentID, AutoStopResumeHandler,
	).Request(ctx, AutoStopRuntimeRequest{RuntimeID: runtimeID})
	return err
}

func (client *Client) SendAutoStopPolicyRefresh(ctx context.Context, environmentID, idempotencyKey string) error {
	_, err := ingress.ObjectSend[AutoStopRefreshSignal](
		client.ingress, AutoStopService, environmentID, AutoStopRefreshHandler,
	).Send(ctx, AutoStopRefreshSignal{}, restate.WithIdempotencyKey(idempotencyKey))
	return err
}
