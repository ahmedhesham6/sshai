package application

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

type AutoStopPolicyRefreshOutbox interface {
	PendingAutoStopPolicyRefresh(context.Context, string) (domain.AutoStopPolicyRefresh, bool, error)
	PendingAutoStopPolicyRefreshes(context.Context, int) ([]domain.AutoStopPolicyRefresh, error)
	AcknowledgeAutoStopPolicyRefresh(context.Context, string, uint64) error
	DeferAutoStopPolicyRefresh(context.Context, string, uint64, time.Time) error
}

type AutoStopPolicyRefreshSender interface {
	SendAutoStopPolicyRefresh(context.Context, string, string) error
}

type AutoStopPolicyRefreshDispatcher struct {
	outbox AutoStopPolicyRefreshOutbox
	sender AutoStopPolicyRefreshSender
	now    func() time.Time
}

func NewAutoStopPolicyRefreshDispatcher(outbox AutoStopPolicyRefreshOutbox, sender AutoStopPolicyRefreshSender) *AutoStopPolicyRefreshDispatcher {
	return &AutoStopPolicyRefreshDispatcher{outbox: outbox, sender: sender, now: time.Now}
}

func NewAutoStopPolicyRefreshRecovery(dispatcher *AutoStopPolicyRefreshDispatcher, interval time.Duration, batchSize int, report func(error)) *WorkflowRecovery {
	return &WorkflowRecovery{dispatchPending: dispatcher.DispatchPendingAutoStopPolicyRefreshes, interval: interval, batchSize: batchSize, report: report}
}

func (dispatcher *AutoStopPolicyRefreshDispatcher) DispatchAutoStopPolicyRefresh(ctx context.Context, environmentID string) error {
	if dispatcher == nil || dispatcher.outbox == nil || dispatcher.sender == nil || !canonicalIdentity(environmentID) {
		return errors.New("dispatch Auto-stop Policy refresh: dispatcher and Environment are required")
	}
	refresh, pending, err := dispatcher.outbox.PendingAutoStopPolicyRefresh(ctx, environmentID)
	if err != nil {
		return fmt.Errorf("dispatch Auto-stop Policy refresh: read pending generation: %w", err)
	}
	if !pending {
		return nil
	}
	return dispatcher.dispatch(ctx, refresh)
}

func (dispatcher *AutoStopPolicyRefreshDispatcher) DispatchPendingAutoStopPolicyRefreshes(ctx context.Context, limit int) error {
	if dispatcher == nil || dispatcher.outbox == nil || dispatcher.sender == nil || limit < 1 {
		return errors.New("dispatch pending Auto-stop Policy refreshes: dispatcher and positive limit are required")
	}
	refreshes, err := dispatcher.outbox.PendingAutoStopPolicyRefreshes(ctx, limit)
	if err != nil {
		return fmt.Errorf("dispatch pending Auto-stop Policy refreshes: read outbox: %w", err)
	}
	var failures []error
	for _, refresh := range refreshes {
		if err := dispatcher.dispatch(ctx, refresh); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (dispatcher *AutoStopPolicyRefreshDispatcher) dispatch(ctx context.Context, refresh domain.AutoStopPolicyRefresh) error {
	if !canonicalIdentity(refresh.EnvironmentID) || refresh.Generation == 0 {
		return errors.New("dispatch Auto-stop Policy refresh: invalid pending refresh")
	}
	key := "auto-stop-policy-refresh:" + refresh.EnvironmentID + ":" + strconv.FormatUint(refresh.Generation, 10)
	if err := dispatcher.sender.SendAutoStopPolicyRefresh(ctx, refresh.EnvironmentID, key); err != nil {
		failure := fmt.Errorf("dispatch Auto-stop Policy refresh %q generation %d: %w", refresh.EnvironmentID, refresh.Generation, err)
		return dispatcher.deferRefresh(ctx, refresh, failure)
	}
	if err := dispatcher.outbox.AcknowledgeAutoStopPolicyRefresh(ctx, refresh.EnvironmentID, refresh.Generation); err != nil {
		failure := fmt.Errorf("acknowledge Auto-stop Policy refresh %q generation %d: %w", refresh.EnvironmentID, refresh.Generation, err)
		return dispatcher.deferRefresh(ctx, refresh, failure)
	}
	return nil
}

func (dispatcher *AutoStopPolicyRefreshDispatcher) deferRefresh(ctx context.Context, refresh domain.AutoStopPolicyRefresh, failure error) error {
	if err := dispatcher.outbox.DeferAutoStopPolicyRefresh(ctx, refresh.EnvironmentID, refresh.Generation, dispatcher.now().Round(0).UTC()); err != nil {
		return errors.Join(failure, fmt.Errorf("defer Auto-stop Policy refresh %q generation %d: %w", refresh.EnvironmentID, refresh.Generation, err))
	}
	return failure
}
