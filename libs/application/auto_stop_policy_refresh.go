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
}

type AutoStopPolicyRefreshSender interface {
	SendAutoStopPolicyRefresh(context.Context, string, string) error
}

type AutoStopPolicyRefreshDispatcher struct {
	outbox AutoStopPolicyRefreshOutbox
	sender AutoStopPolicyRefreshSender
}

func NewAutoStopPolicyRefreshDispatcher(outbox AutoStopPolicyRefreshOutbox, sender AutoStopPolicyRefreshSender) *AutoStopPolicyRefreshDispatcher {
	return &AutoStopPolicyRefreshDispatcher{outbox: outbox, sender: sender}
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
	for _, refresh := range refreshes {
		if err := dispatcher.dispatch(ctx, refresh); err != nil {
			return err
		}
	}
	return nil
}

func (dispatcher *AutoStopPolicyRefreshDispatcher) dispatch(ctx context.Context, refresh domain.AutoStopPolicyRefresh) error {
	if !canonicalIdentity(refresh.EnvironmentID) || refresh.Generation == 0 {
		return errors.New("dispatch Auto-stop Policy refresh: invalid pending refresh")
	}
	key := "auto-stop-policy-refresh:" + refresh.EnvironmentID + ":" + strconv.FormatUint(refresh.Generation, 10)
	if err := dispatcher.sender.SendAutoStopPolicyRefresh(ctx, refresh.EnvironmentID, key); err != nil {
		return fmt.Errorf("dispatch Auto-stop Policy refresh %q generation %d: %w", refresh.EnvironmentID, refresh.Generation, err)
	}
	if err := dispatcher.outbox.AcknowledgeAutoStopPolicyRefresh(ctx, refresh.EnvironmentID, refresh.Generation); err != nil {
		return fmt.Errorf("acknowledge Auto-stop Policy refresh %q generation %d: %w", refresh.EnvironmentID, refresh.Generation, err)
	}
	return nil
}
