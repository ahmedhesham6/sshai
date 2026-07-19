package workflows

import (
	"errors"
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"
)

type durableDeadlinePollConfig struct {
	startedAt      time.Time
	timeout        time.Duration
	initialDelay   time.Duration
	maxDelay       time.Duration
	stepPrefix     string
	readStepPrefix string
	now            func() time.Time
}

type durableDeadlinePollValue[T any] struct {
	value     T
	checkedAt time.Time
}

type durableDeadlinePollRead[T any] struct {
	Value            T
	UseValue         bool
	RetryableFailure bool
}

type durableDeadlinePollResult[T any] struct {
	value     T
	hasValue  bool
	startedAt time.Time
	checkedAt time.Time
	timedOut  bool
}

// durableDeadlinePoll owns the durable-deadline invariant shared by every
// workflow poller: one journaled start, one absolute deadline, and one
// journaled clock observation after each external read. Retryable read failures
// must be returned as durableDeadlinePollRead values so they advance the
// deadline instead of causing Restate to retry one action forever.
func durableDeadlinePoll[T any](
	ctx restate.Context,
	initial *durableDeadlinePollValue[T],
	config durableDeadlinePollConfig,
	read func(restate.RunContext, time.Time) (durableDeadlinePollRead[T], error),
	evaluate func(T, time.Time) (T, bool),
	stamp func(T, time.Time) T,
) (durableDeadlinePollResult[T], error) {
	if config.timeout <= 0 || config.initialDelay <= 0 || config.maxDelay <= 0 || config.stepPrefix == "" || config.readStepPrefix == "" || config.now == nil || read == nil || evaluate == nil {
		return durableDeadlinePollResult[T]{}, restate.TerminalErrorf("durable deadline poll is not configured")
	}
	startedAt := config.startedAt
	if startedAt.IsZero() {
		var err error
		startedAt, err = restate.Run(ctx, func(restate.RunContext) (time.Time, error) {
			return config.now().Round(0).UTC(), nil
		}, restate.WithName(config.stepPrefix+"-poll-started-at"))
		if err != nil {
			return durableDeadlinePollResult[T]{}, err
		}
	}
	if startedAt.IsZero() {
		return durableDeadlinePollResult[T]{}, restate.TerminalErrorf("durable deadline poll start time is required")
	}
	deadline := startedAt.Add(config.timeout)
	result := durableDeadlinePollResult[T]{startedAt: startedAt, checkedAt: startedAt}
	if initial != nil {
		if initial.checkedAt.IsZero() {
			return durableDeadlinePollResult[T]{}, restate.TerminalErrorf("durable deadline poll initial observation time is required")
		}
		result.value, result.hasValue, result.checkedAt = initial.value, true, initial.checkedAt
		var done bool
		result.value, done = evaluate(result.value, startedAt)
		if done {
			return result, nil
		}
	}

	delay := config.initialDelay
	readImmediately := initial == nil
	for attempt := 1; ; attempt++ {
		if !result.checkedAt.Before(deadline) {
			result.timedOut = true
			return result, nil
		}
		sleptBeforeRead := !readImmediately
		if sleptBeforeRead {
			remaining := deadline.Sub(result.checkedAt)
			if delay > remaining {
				delay = remaining
			}
			if err := restate.Sleep(ctx, delay, restate.WithName(fmt.Sprintf("%s-wait-%d", config.stepPrefix, attempt))); err != nil {
				return durableDeadlinePollResult[T]{}, err
			}
		}
		readImmediately = false

		type journaledRead struct {
			Read      durableDeadlinePollRead[T]
			CheckedAt time.Time
		}
		journaled, err := restate.Run(ctx, func(runCtx restate.RunContext) (journaledRead, error) {
			attempted, err := read(runCtx, startedAt)
			if err != nil {
				return journaledRead{}, err
			}
			return journaledRead{Read: attempted, CheckedAt: config.now().Round(0).UTC()}, nil
		}, restate.WithName(fmt.Sprintf("%s-%d", config.readStepPrefix, attempt)))
		if err != nil {
			return durableDeadlinePollResult[T]{}, err
		}
		if journaled.CheckedAt.IsZero() {
			return durableDeadlinePollResult[T]{}, restate.TerminalErrorf("durable deadline poll read time is required")
		}
		if journaled.CheckedAt.Before(result.checkedAt) {
			return durableDeadlinePollResult[T]{}, restate.ToTerminalError(errors.New("durable deadline poll clock moved backwards"))
		}
		result.checkedAt = journaled.CheckedAt
		if journaled.Read.UseValue {
			result.value, result.hasValue = journaled.Read.Value, true
			if stamp != nil {
				result.value = stamp(result.value, result.checkedAt)
			}
		}
		if !journaled.Read.RetryableFailure && result.hasValue {
			var done bool
			result.value, done = evaluate(result.value, startedAt)
			if done {
				return result, nil
			}
		}
		if !result.checkedAt.Before(deadline) {
			result.timedOut = true
			return result, nil
		}
		if sleptBeforeRead {
			delay = min(delay*2, config.maxDelay)
		}
	}
}
