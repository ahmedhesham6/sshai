package workflows

import (
	"context"
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

const (
	AutoStopService         = "EnvironmentAutoStop"
	AutoStopObserveHandler  = "Observe"
	AutoStopExpireHandler   = "Expire"
	AutoStopSuppressHandler = "Suppress"
	AutoStopResumeHandler   = "Resume"
	AutoStopRefreshHandler  = "Refresh"
	autoStopStateKey        = "coordination"
	autoStopStaleThreshold  = 5 * time.Minute
)

type AutoStopRefreshRequest struct {
	EnvironmentID         string
	RuntimeID             string
	AfterSnapshotSequence uint64
	FreshAfter            time.Time
}

type AutoStopSnapshotSource interface {
	RefreshAutoStop(context.Context, AutoStopRefreshRequest) (AutoStopObservation, error)
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
	observation, err := restate.Run(ctx, func(runCtx restate.RunContext) (AutoStopObservation, error) {
		return object.source.RefreshAutoStop(runCtx, AutoStopRefreshRequest{
			EnvironmentID: state.EnvironmentID, RuntimeID: state.RuntimeID,
			FreshAfter: processedAt.Add(-autoStopStaleThreshold),
		})
	}, restate.WithName("refresh-auto-stop-policy"))
	if err != nil {
		return AutoStopTransition{}, err
	}
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
	observation, err := restate.Run(ctx, func(runCtx restate.RunContext) (AutoStopObservation, error) {
		freshAfter := time.Now().UTC()
		observation, err := object.source.RefreshAutoStop(runCtx, AutoStopRefreshRequest{
			EnvironmentID: state.EnvironmentID, RuntimeID: state.RuntimeID,
			AfterSnapshotSequence: state.LastSnapshotSequence, FreshAfter: freshAfter,
		})
		observation.FreshAfter = freshAfter
		observation.ProcessedAt = time.Now().UTC()
		return observation, err
	}, restate.WithName("refresh-activity-snapshot"))
	if err != nil {
		return AutoStopTransition{}, err
	}
	transition, evaluationErr := (AutoStopCoordinator{}).Expire(state, AutoStopExpiry{
		RuntimeID: timer.RuntimeID, Generation: timer.Generation, Observation: observation,
	})
	if evaluationErr != nil {
		return AutoStopTransition{}, restate.TerminalErrorf("invalid Auto-stop expiry: %v", evaluationErr)
	}
	if transition.Stop != nil {
		request := *transition.Stop
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
			return object.dispatcher.DispatchRuntimeStop(runCtx, request)
		}, restate.WithName("dispatch-runtime-stop")); err != nil {
			return AutoStopTransition{}, err
		}
	}
	restate.Set(ctx, autoStopStateKey, transition.State)
	object.schedule(ctx, transition.Timer)
	return transition, nil
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
