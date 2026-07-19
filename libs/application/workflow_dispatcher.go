package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

type EnvironmentCreateOutbox interface {
	PendingEnvironmentCreate(context.Context, string) (domain.EnvironmentCreateDispatch, bool, error)
	PendingEnvironmentCreates(context.Context, int) ([]domain.EnvironmentCreateDispatch, error)
}

type WorkflowRecovery struct {
	dispatchPending func(context.Context, int) error
	interval        time.Duration
	batchSize       int
	report          func(error)
}

func NewWorkflowRecovery(dispatcher *WorkflowDispatcher, interval time.Duration, batchSize int, report func(error)) *WorkflowRecovery {
	return &WorkflowRecovery{dispatchPending: dispatcher.DispatchPendingEnvironmentCreates, interval: interval, batchSize: batchSize, report: report}
}

func NewRuntimeWorkflowRecovery(dispatcher *RuntimeWorkflowDispatcher, interval time.Duration, batchSize int, report func(error)) *WorkflowRecovery {
	return &WorkflowRecovery{dispatchPending: dispatcher.DispatchPendingRuntimeOperations, interval: interval, batchSize: batchSize, report: report}
}

func (recovery *WorkflowRecovery) Run(ctx context.Context) error {
	if recovery.dispatchPending == nil || recovery.interval <= 0 || recovery.batchSize < 1 {
		return errors.New("run workflow recovery: interval and batch size must be positive")
	}
	ticker := time.NewTicker(recovery.interval)
	defer ticker.Stop()
	for {
		if err := recovery.dispatchPending(ctx, recovery.batchSize); err != nil && recovery.report != nil {
			recovery.report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type EnvironmentCreateSender interface {
	SendEnvironmentCreate(context.Context, domain.EnvironmentCreateDispatch) error
}

type RuntimeOperationOutbox interface {
	PendingRuntimeOperation(context.Context, string) (domain.RuntimeOperationDispatch, bool, error)
	PendingRuntimeOperations(context.Context, int) ([]domain.RuntimeOperationDispatch, error)
}

type RuntimeOperationSender interface {
	SendRuntimeOperation(context.Context, domain.RuntimeOperationDispatch) error
}

type WorkflowDispatcher struct {
	outbox EnvironmentCreateOutbox
	sender EnvironmentCreateSender
}

func NewEnvironmentCreateDispatcher(outbox EnvironmentCreateOutbox, sender EnvironmentCreateSender) *WorkflowDispatcher {
	return &WorkflowDispatcher{outbox: outbox, sender: sender}
}

func (dispatcher *WorkflowDispatcher) DispatchEnvironmentCreate(ctx context.Context, operationID string) error {
	input, pending, err := dispatcher.outbox.PendingEnvironmentCreate(ctx, operationID)
	if err != nil {
		return fmt.Errorf("dispatch Environment create: read outbox: %w", err)
	}
	if !pending {
		return nil
	}
	if err := dispatcher.sender.SendEnvironmentCreate(ctx, input); err != nil {
		return fmt.Errorf("dispatch Environment create: %w", err)
	}
	return nil
}

func (dispatcher *WorkflowDispatcher) DispatchPendingEnvironmentCreates(ctx context.Context, limit int) error {
	inputs, err := dispatcher.outbox.PendingEnvironmentCreates(ctx, limit)
	if err != nil {
		return fmt.Errorf("dispatch pending Environment creates: read outbox: %w", err)
	}
	for _, input := range inputs {
		if err := dispatcher.sender.SendEnvironmentCreate(ctx, input); err != nil {
			return fmt.Errorf("dispatch pending Environment create %q: %w", input.OperationID, err)
		}
	}
	return nil
}

type RuntimeWorkflowDispatcher struct {
	outbox RuntimeOperationOutbox
	sender RuntimeOperationSender
}

func NewRuntimeOperationDispatcher(outbox RuntimeOperationOutbox, sender RuntimeOperationSender) *RuntimeWorkflowDispatcher {
	return &RuntimeWorkflowDispatcher{outbox: outbox, sender: sender}
}

func (dispatcher *RuntimeWorkflowDispatcher) DispatchRuntimeOperation(ctx context.Context, operationID string) error {
	input, pending, err := dispatcher.outbox.PendingRuntimeOperation(ctx, operationID)
	if err != nil {
		return fmt.Errorf("dispatch Runtime Operation: read outbox: %w", err)
	}
	if !pending {
		return nil
	}
	if err := dispatcher.sender.SendRuntimeOperation(ctx, input); err != nil {
		return fmt.Errorf("dispatch Runtime Operation: %w", err)
	}
	return nil
}

func (dispatcher *RuntimeWorkflowDispatcher) DispatchPendingRuntimeOperations(ctx context.Context, limit int) error {
	inputs, err := dispatcher.outbox.PendingRuntimeOperations(ctx, limit)
	if err != nil {
		return fmt.Errorf("dispatch pending Runtime Operations: read outbox: %w", err)
	}
	var failures []error
	for _, input := range inputs {
		if err := dispatcher.sender.SendRuntimeOperation(ctx, input); err != nil {
			failures = append(failures, fmt.Errorf("dispatch pending Runtime Operation %q: %w", input.OperationID, err))
		}
	}
	return errors.Join(failures...)
}
