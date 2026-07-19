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

func TestAutoStopPolicyRefreshDispatcherContinuesAfterPerItemFailure(t *testing.T) {
	refreshes := []domain.AutoStopPolicyRefresh{
		{EnvironmentID: "environment-1", Generation: 1},
		{EnvironmentID: "environment-2", Generation: 2},
		{EnvironmentID: "environment-3", Generation: 3},
	}
	outbox := &autoStopPolicyRefreshOutboxFake{pending: append([]domain.AutoStopPolicyRefresh(nil), refreshes...)}
	sender := &autoStopPolicyRefreshSenderFake{errorsByEnvironment: map[string]error{
		"environment-1": errors.New("first unavailable"),
		"environment-3": errors.New("third unavailable"),
	}}
	dispatcher := application.NewAutoStopPolicyRefreshDispatcher(outbox, sender)

	err := dispatcher.DispatchPendingAutoStopPolicyRefreshes(t.Context(), len(refreshes))
	if err == nil || !strings.Contains(err.Error(), "environment-1") || !strings.Contains(err.Error(), "environment-3") {
		t.Fatalf("DispatchPendingAutoStopPolicyRefreshes() error = %v, want both item failures", err)
	}
	if len(sender.environments) != len(refreshes) || len(outbox.acknowledged) != 1 || outbox.acknowledged[0] != refreshes[1] {
		t.Fatalf("dispatches/acknowledgements = %#v/%#v, want every dispatch and only successful acknowledgement", sender.environments, outbox.acknowledged)
	}
}

func TestAutoStopPolicyRefreshDispatcherBacksOffPoisonedRowSoNewerWorkRuns(t *testing.T) {
	refreshes := []domain.AutoStopPolicyRefresh{
		{EnvironmentID: "environment-poison", Generation: 1},
		{EnvironmentID: "environment-new", Generation: 1},
	}
	outbox := &autoStopPolicyRefreshOutboxFake{pending: refreshes, deferred: map[string]int{}}
	sender := &autoStopPolicyRefreshSenderFake{errorsByEnvironment: map[string]error{
		"environment-poison": errors.New("permanent dispatch rejection"),
	}}
	dispatcher := application.NewAutoStopPolicyRefreshDispatcher(outbox, sender)

	if err := dispatcher.DispatchPendingAutoStopPolicyRefreshes(t.Context(), 1); err == nil {
		t.Fatal("first poisoned refresh error = nil")
	}
	if err := dispatcher.DispatchPendingAutoStopPolicyRefreshes(t.Context(), 1); err != nil {
		t.Fatalf("newer refresh dispatch: %v", err)
	}
	if len(sender.environments) != 2 || sender.environments[1] != "environment-new" {
		t.Fatalf("refresh attempts = %#v, want poisoned row then newer row", sender.environments)
	}
}

type autoStopPolicyRefreshOutboxFake struct {
	pending      []domain.AutoStopPolicyRefresh
	acknowledged []domain.AutoStopPolicyRefresh
	cancel       context.CancelFunc
	deferred     map[string]int
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
	var eligible []domain.AutoStopPolicyRefresh
	for _, refresh := range fake.pending {
		if fake.deferred[refresh.EnvironmentID] > 0 {
			fake.deferred[refresh.EnvironmentID]--
			continue
		}
		eligible = append(eligible, refresh)
	}
	if limit < len(eligible) {
		return append([]domain.AutoStopPolicyRefresh(nil), eligible[:limit]...), nil
	}
	return append([]domain.AutoStopPolicyRefresh(nil), eligible...), nil
}

func (fake *autoStopPolicyRefreshOutboxFake) DeferAutoStopPolicyRefresh(_ context.Context, environmentID string, _ uint64, _ time.Time) error {
	if fake.deferred == nil {
		fake.deferred = make(map[string]int)
	}
	fake.deferred[environmentID] = 1
	return nil
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
	keys                []string
	environments        []string
	err                 error
	errorsByEnvironment map[string]error
}

func (fake *autoStopPolicyRefreshSenderFake) SendAutoStopPolicyRefresh(_ context.Context, environmentID string, key string) error {
	fake.keys = append(fake.keys, key)
	fake.environments = append(fake.environments, environmentID)
	if err := fake.errorsByEnvironment[environmentID]; err != nil {
		return err
	}
	return fake.err
}
