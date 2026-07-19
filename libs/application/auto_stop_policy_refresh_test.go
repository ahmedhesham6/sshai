package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestAutoStopPolicyRefreshRecoveryClosesCommitDispatchCrashWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	pending := domain.AutoStopPolicyRefresh{EnvironmentID: "environment-1", Generation: 4}
	outbox := &autoStopPolicyRefreshOutboxFake{pending: []domain.AutoStopPolicyRefresh{pending}, cancel: cancel}
	sender := &autoStopPolicyRefreshSenderFake{err: errors.New("Restate unavailable")}
	dispatcher := application.NewAutoStopPolicyRefreshDispatcher(outbox, sender)

	if err := dispatcher.DispatchAutoStopPolicyRefresh(ctx, pending.EnvironmentID); err == nil {
		t.Fatal("fast-path DispatchAutoStopPolicyRefresh() error = nil")
	}
	if len(outbox.pending) != 1 || len(outbox.acknowledged) != 0 {
		t.Fatalf("failed fast path lost refresh: pending=%#v acknowledged=%#v", outbox.pending, outbox.acknowledged)
	}
	sender.err = nil
	recovery := application.NewAutoStopPolicyRefreshRecovery(dispatcher, time.Millisecond, 10, nil)
	if err := recovery.Run(ctx); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if len(sender.keys) != 2 || sender.keys[0] != sender.keys[1] {
		t.Fatalf("refresh idempotency keys = %#v", sender.keys)
	}
	if len(outbox.pending) != 0 || len(outbox.acknowledged) != 1 || outbox.acknowledged[0] != pending {
		t.Fatalf("recovered refresh: pending=%#v acknowledged=%#v", outbox.pending, outbox.acknowledged)
	}
}

type autoStopPolicyRefreshOutboxFake struct {
	pending      []domain.AutoStopPolicyRefresh
	acknowledged []domain.AutoStopPolicyRefresh
	cancel       context.CancelFunc
}

func (fake *autoStopPolicyRefreshOutboxFake) PendingAutoStopPolicyRefresh(_ context.Context, environmentID string) (domain.AutoStopPolicyRefresh, bool, error) {
	for _, refresh := range fake.pending {
		if refresh.EnvironmentID == environmentID {
			return refresh, true, nil
		}
	}
	return domain.AutoStopPolicyRefresh{}, false, nil
}

func (fake *autoStopPolicyRefreshOutboxFake) PendingAutoStopPolicyRefreshes(_ context.Context, limit int) ([]domain.AutoStopPolicyRefresh, error) {
	if limit < len(fake.pending) {
		return append([]domain.AutoStopPolicyRefresh(nil), fake.pending[:limit]...), nil
	}
	return append([]domain.AutoStopPolicyRefresh(nil), fake.pending...), nil
}

func (fake *autoStopPolicyRefreshOutboxFake) AcknowledgeAutoStopPolicyRefresh(_ context.Context, environmentID string, generation uint64) error {
	refresh := domain.AutoStopPolicyRefresh{EnvironmentID: environmentID, Generation: generation}
	fake.acknowledged = append(fake.acknowledged, refresh)
	for index, candidate := range fake.pending {
		if candidate == refresh {
			fake.pending = append(fake.pending[:index], fake.pending[index+1:]...)
			break
		}
	}
	if fake.cancel != nil {
		fake.cancel()
	}
	return nil
}

type autoStopPolicyRefreshSenderFake struct {
	keys []string
	err  error
}

func (fake *autoStopPolicyRefreshSenderFake) SendAutoStopPolicyRefresh(_ context.Context, _ string, key string) error {
	fake.keys = append(fake.keys, key)
	return fake.err
}
