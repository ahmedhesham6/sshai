package application_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestRuntimeOperationDispatcherRecoversPersistedTarget(t *testing.T) {
	input := domain.RuntimeOperationDispatch{
		OperationID: "operation-1", OperationType: domain.OperationRuntimeReplace,
		EnvironmentID: "environment-1", RuntimeID: "runtime-7",
	}
	outbox := &runtimeOperationOutboxFake{pending: []domain.RuntimeOperationDispatch{input}}
	sender := &runtimeOperationSenderFake{err: errors.New("Restate unavailable")}
	dispatcher := application.NewRuntimeOperationDispatcher(outbox, sender)

	if err := dispatcher.DispatchRuntimeOperation(t.Context(), "operation-1"); err == nil {
		t.Fatal("DispatchRuntimeOperation() error = nil")
	}
	if len(outbox.pending) != 1 {
		t.Fatal("failed Runtime dispatch lost durable command")
	}
	sender.err = nil
	sender.onSend = func(domain.RuntimeOperationDispatch) { outbox.pending = nil }
	if err := dispatcher.DispatchPendingRuntimeOperations(t.Context(), 10); err != nil {
		t.Fatalf("DispatchPendingRuntimeOperations(): %v", err)
	}
	if len(sender.inputs) != 2 || sender.inputs[1] != input || len(outbox.pending) != 0 {
		t.Fatalf("recovered Runtime dispatches = %#v pending = %#v", sender.inputs, outbox.pending)
	}
}

func TestRuntimeOperationDispatcherIgnoresStartedOrMissingOperation(t *testing.T) {
	outbox := &runtimeOperationOutboxFake{}
	sender := &runtimeOperationSenderFake{}
	dispatcher := application.NewRuntimeOperationDispatcher(outbox, sender)

	if err := dispatcher.DispatchRuntimeOperation(t.Context(), "operation-started"); err != nil {
		t.Fatalf("DispatchRuntimeOperation(): %v", err)
	}
	if len(sender.inputs) != 0 || len(outbox.requested) != 1 || outbox.requested[0] != "operation-started" {
		t.Fatalf("started Runtime dispatch = requested:%#v sent:%#v", outbox.requested, sender.inputs)
	}
}

func TestRuntimeOperationDispatcherContinuesAfterPerItemFailure(t *testing.T) {
	inputs := []domain.RuntimeOperationDispatch{
		{OperationID: "operation-1", OperationType: domain.OperationRuntimeStart},
		{OperationID: "operation-2", OperationType: domain.OperationRuntimeStop},
		{OperationID: "operation-3", OperationType: domain.OperationRuntimeReplace},
	}
	outbox := &runtimeOperationOutboxFake{pending: inputs}
	sender := &runtimeOperationSenderFake{errorsByOperation: map[string]error{
		"operation-1": errors.New("first unavailable"),
		"operation-3": errors.New("third unavailable"),
	}}
	dispatcher := application.NewRuntimeOperationDispatcher(outbox, sender)

	err := dispatcher.DispatchPendingRuntimeOperations(t.Context(), len(inputs))
	if err == nil || !strings.Contains(err.Error(), "operation-1") || !strings.Contains(err.Error(), "operation-3") {
		t.Fatalf("DispatchPendingRuntimeOperations() error = %v, want both item failures", err)
	}
	if len(sender.inputs) != len(inputs) || sender.inputs[1].OperationID != "operation-2" {
		t.Fatalf("dispatch attempts = %#v, want all inputs in order", sender.inputs)
	}
}

func TestRuntimeWorkflowRecoveryRetriesPendingOperationsUntilCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	input := domain.RuntimeOperationDispatch{OperationID: "operation-1", OperationType: domain.OperationRuntimeStart}
	outbox := &runtimeOperationOutboxFake{pending: []domain.RuntimeOperationDispatch{input}}
	sender := &runtimeOperationSenderFake{err: errors.New("Restate unavailable")}
	sender.onSend = func(domain.RuntimeOperationDispatch) {
		if len(sender.inputs) == 2 {
			outbox.pending = nil
			cancel()
		}
	}
	dispatcher := application.NewRuntimeOperationDispatcher(outbox, sender)
	var reported []error
	recovery := application.NewRuntimeWorkflowRecovery(dispatcher, time.Millisecond, 10, func(err error) {
		reported = append(reported, err)
		sender.err = nil
	})

	if err := recovery.Run(ctx); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if len(sender.inputs) != 2 || len(outbox.pending) != 0 || len(reported) != 1 {
		t.Fatalf("Runtime recovery inputs=%d pending=%d reported=%d", len(sender.inputs), len(outbox.pending), len(reported))
	}
}

type runtimeOperationOutboxFake struct {
	pending   []domain.RuntimeOperationDispatch
	requested []string
}

func (outbox *runtimeOperationOutboxFake) PendingRuntimeOperation(_ context.Context, operationID string) (domain.RuntimeOperationDispatch, bool, error) {
	outbox.requested = append(outbox.requested, operationID)
	for _, input := range outbox.pending {
		if input.OperationID == operationID {
			return input, true, nil
		}
	}
	return domain.RuntimeOperationDispatch{}, false, nil
}

func (outbox *runtimeOperationOutboxFake) PendingRuntimeOperations(_ context.Context, limit int) ([]domain.RuntimeOperationDispatch, error) {
	if limit < len(outbox.pending) {
		return append([]domain.RuntimeOperationDispatch(nil), outbox.pending[:limit]...), nil
	}
	return append([]domain.RuntimeOperationDispatch(nil), outbox.pending...), nil
}

type runtimeOperationSenderFake struct {
	inputs            []domain.RuntimeOperationDispatch
	err               error
	errorsByOperation map[string]error
	onSend            func(domain.RuntimeOperationDispatch)
}

func (sender *runtimeOperationSenderFake) SendRuntimeOperation(_ context.Context, input domain.RuntimeOperationDispatch) error {
	sender.inputs = append(sender.inputs, input)
	if err := sender.errorsByOperation[input.OperationID]; err != nil {
		return err
	}
	if sender.err == nil && sender.onSend != nil {
		sender.onSend(input)
	}
	return sender.err
}
