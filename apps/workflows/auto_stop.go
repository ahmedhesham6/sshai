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
	autoStopStateKey        = "coordination"
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
		Handler(AutoStopResumeHandler, restate.NewObjectHandler(object.Resume))
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
	transition, err := (AutoStopCoordinator{}).Observe(state, input)
	if err != nil {
		return AutoStopTransition{}, restate.TerminalErrorf("invalid Auto-stop observation: %v", err)
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
