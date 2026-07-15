package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

type AutoStopCoordinationState struct {
	EnvironmentID        string
	RuntimeID            string
	PolicyID             string
	PolicyMode           domain.AutoStopMode
	PolicyGraceSeconds   int
	PolicyGeneration     uint64
	TimerGeneration      uint64
	TimerPending         bool
	LastSnapshotSequence uint64
	DispatchedGeneration uint64
}

type AutoStopObservation struct {
	RuntimeID        string
	Policy           domain.AutoStopPolicySnapshot
	PolicyGeneration uint64
	FreshAfter       time.Time
	Snapshot         *domain.AutoStopActivitySnapshot
	Conflicts        []domain.AutoStopConflict
}

type AutoStopTimer struct {
	RuntimeID  string
	Generation uint64
	Delay      time.Duration
}

type RuntimeStopRequest struct {
	EnvironmentID  string
	RuntimeID      string
	Reason         string
	IdempotencyKey string
}

const RuntimeStopReasonAutoStop = "auto_stop"

type AutoStopTransition struct {
	State     AutoStopCoordinationState
	Decision  domain.AutoStopDecision
	Timer     *AutoStopTimer
	Stop      *RuntimeStopRequest
	Cancelled bool
}

type AutoStopExpiry struct {
	RuntimeID   string
	Generation  uint64
	Observation AutoStopObservation
}

type AutoStopCoordinator struct{}

func (AutoStopCoordinator) Observe(state AutoStopCoordinationState, observation AutoStopObservation) (AutoStopTransition, error) {
	transition := AutoStopTransition{State: state}
	if coordinationChanged(state, observation) {
		transition.Cancelled = state.TimerPending
		if state.TimerPending {
			transition.State.TimerGeneration++
		}
		transition.State.RuntimeID = observation.RuntimeID
		transition.State.PolicyID = observation.Policy.ID
		transition.State.PolicyMode = observation.Policy.Mode
		transition.State.PolicyGraceSeconds = observation.Policy.GracePeriodSeconds
		transition.State.PolicyGeneration = observation.PolicyGeneration
		transition.State.TimerPending = false
		transition.State.LastSnapshotSequence = 0
		transition.State.DispatchedGeneration = 0
	}
	decision, err := evaluateAutoStop(transition.State, observation)
	if err != nil {
		return AutoStopTransition{}, err
	}
	transition.Decision = decision
	if decision.SnapshotSequence > transition.State.LastSnapshotSequence {
		transition.State.LastSnapshotSequence = decision.SnapshotSequence
	}
	if !decision.Qualifies {
		if transition.State.TimerPending {
			transition.Cancelled = true
			transition.State.TimerGeneration++
			transition.State.TimerPending = false
		}
		return transition, nil
	}
	if transition.State.TimerPending || transition.State.DispatchedGeneration != 0 {
		return transition, nil
	}
	transition.State.TimerGeneration++
	transition.State.TimerPending = true
	transition.Timer = &AutoStopTimer{
		RuntimeID: observation.RuntimeID, Generation: transition.State.TimerGeneration,
		Delay: time.Duration(observation.Policy.GracePeriodSeconds) * time.Second,
	}
	return transition, nil
}

func (coordinator AutoStopCoordinator) Expire(state AutoStopCoordinationState, expiry AutoStopExpiry) (AutoStopTransition, error) {
	if !state.TimerPending || expiry.Generation != state.TimerGeneration || expiry.RuntimeID != state.RuntimeID {
		return AutoStopTransition{State: state}, nil
	}
	if coordinationChanged(state, expiry.Observation) {
		transition, err := coordinator.Observe(state, expiry.Observation)
		if err != nil {
			return AutoStopTransition{}, err
		}
		transition.Cancelled = true
		return transition, nil
	}
	decision, err := evaluateAutoStop(state, expiry.Observation)
	if err != nil {
		return AutoStopTransition{}, err
	}
	next := state
	next.TimerPending = false
	if decision.SnapshotSequence > next.LastSnapshotSequence {
		next.LastSnapshotSequence = decision.SnapshotSequence
	}
	transition := AutoStopTransition{State: next, Decision: decision}
	if !decision.Qualifies {
		return transition, nil
	}
	next.DispatchedGeneration = expiry.Generation
	transition.State = next
	transition.Stop = &RuntimeStopRequest{
		EnvironmentID: state.EnvironmentID, RuntimeID: state.RuntimeID, Reason: RuntimeStopReasonAutoStop,
		IdempotencyKey: fmt.Sprintf("runtime.stop:%s:%s:%s:%d", RuntimeStopReasonAutoStop, state.EnvironmentID, state.RuntimeID, expiry.Generation),
	}
	return transition, nil
}

func evaluateAutoStop(state AutoStopCoordinationState, observation AutoStopObservation) (domain.AutoStopDecision, error) {
	policy, err := restoreAutoStopPolicy(state.EnvironmentID, observation)
	if err != nil {
		return domain.AutoStopDecision{}, err
	}
	return policy.Evaluate(domain.AutoStopEvaluationRequest{
		RuntimeID: observation.RuntimeID, PreviousSnapshotSequence: state.LastSnapshotSequence,
		FreshAfter: observation.FreshAfter, Snapshot: observation.Snapshot, Conflicts: observation.Conflicts,
	})
}

func restoreAutoStopPolicy(environmentID string, observation AutoStopObservation) (domain.AutoStopPolicy, error) {
	if environmentID == "" || observation.RuntimeID == "" || observation.PolicyGeneration == 0 {
		return domain.AutoStopPolicy{}, errors.New("coordinate Auto-stop: Environment, Runtime, and policy generation are required")
	}
	if observation.Policy.EnvironmentID != environmentID {
		return domain.AutoStopPolicy{}, errors.New("coordinate Auto-stop: Policy belongs to another Environment")
	}
	return domain.NewAutoStopPolicy(
		observation.Policy.ID, observation.Policy.EnvironmentID, observation.Policy.Mode, observation.Policy.GracePeriodSeconds,
	)
}

func coordinationChanged(state AutoStopCoordinationState, observation AutoStopObservation) bool {
	return state.RuntimeID != observation.RuntimeID || state.PolicyID != observation.Policy.ID ||
		state.PolicyMode != observation.Policy.Mode || state.PolicyGraceSeconds != observation.Policy.GracePeriodSeconds ||
		state.PolicyGeneration != observation.PolicyGeneration
}
