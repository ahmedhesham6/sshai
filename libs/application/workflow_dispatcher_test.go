package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestEnvironmentCreateDispatcherRecoversPendingCommand(t *testing.T) {
	outbox := &outboxFake{pending: []domain.EnvironmentCreateDispatch{{
		OperationID: "operation-1", EnvironmentID: "environment-1", Region: "us-east-1", AvailabilityZone: "us-east-1a",
	}}}
	sender := &senderFake{err: errors.New("Restate unavailable")}
	dispatcher := application.NewEnvironmentCreateDispatcher(outbox, sender)

	if err := dispatcher.DispatchEnvironmentCreate(t.Context(), "operation-1"); err == nil {
		t.Fatal("DispatchEnvironmentCreate() error = nil")
	}
	if len(outbox.pending) != 1 {
		t.Fatal("failed dispatch lost durable command")
	}

	sender.err = nil
	sender.onSend = func(input domain.EnvironmentCreateDispatch) { outbox.pending = nil }
	if err := dispatcher.DispatchPendingEnvironmentCreates(t.Context(), 10); err != nil {
		t.Fatalf("DispatchPendingEnvironmentCreates(): %v", err)
	}
	if len(sender.inputs) != 2 || sender.inputs[1].OperationID != "operation-1" || len(outbox.pending) != 0 {
		t.Fatalf("recovered dispatches = %#v, pending = %#v", sender.inputs, outbox.pending)
	}
}

func TestWorkflowRecoveryRetriesPendingCommandsUntilCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	outbox := &outboxFake{pending: []domain.EnvironmentCreateDispatch{{OperationID: "operation-1"}}}
	sender := &senderFake{errors: []error{errors.New("Restate unavailable"), nil}}
	sender.onSend = func(domain.EnvironmentCreateDispatch) {
		if len(sender.inputs) == 2 {
			outbox.pending = nil
			cancel()
		}
	}
	dispatcher := application.NewEnvironmentCreateDispatcher(outbox, sender)
	var reported []error
	recovery := application.NewWorkflowRecovery(dispatcher, time.Millisecond, 10, func(err error) {
		reported = append(reported, err)
	})

	if err := recovery.Run(ctx); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if len(sender.inputs) != 2 || len(outbox.pending) != 0 || len(reported) != 1 {
		t.Fatalf("recovery inputs=%d pending=%d reported=%d", len(sender.inputs), len(outbox.pending), len(reported))
	}
}

type outboxFake struct {
	pending []domain.EnvironmentCreateDispatch
}

func (outbox *outboxFake) PendingEnvironmentCreate(_ context.Context, operationID string) (domain.EnvironmentCreateDispatch, bool, error) {
	for _, input := range outbox.pending {
		if input.OperationID == operationID {
			return input, true, nil
		}
	}
	return domain.EnvironmentCreateDispatch{}, false, nil
}

func (outbox *outboxFake) PendingEnvironmentCreates(_ context.Context, limit int) ([]domain.EnvironmentCreateDispatch, error) {
	if limit < len(outbox.pending) {
		return outbox.pending[:limit], nil
	}
	return append([]domain.EnvironmentCreateDispatch(nil), outbox.pending...), nil
}

type senderFake struct {
	inputs []domain.EnvironmentCreateDispatch
	err    error
	errors []error
	onSend func(domain.EnvironmentCreateDispatch)
}

func (sender *senderFake) SendEnvironmentCreate(_ context.Context, input domain.EnvironmentCreateDispatch) error {
	sender.inputs = append(sender.inputs, input)
	if len(sender.errors) > 0 {
		sender.err = sender.errors[0]
		sender.errors = sender.errors[1:]
	}
	if sender.err == nil && sender.onSend != nil {
		sender.onSend(input)
	}
	return sender.err
}
