package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestAutoStopPolicyServiceEnforcesCadenceFloor(t *testing.T) {
	tests := []struct {
		name      string
		mode      domain.AutoStopMode
		grace     int
		wantError bool
	}{
		{name: "manual permits zero", mode: domain.AutoStopManual, grace: 0},
		{name: "disconnected rejects below cadence", mode: domain.AutoStopWhenDisconnected, grace: 59, wantError: true},
		{name: "agents reject below cadence", mode: domain.AutoStopWhenAgentsFinish, grace: 1, wantError: true},
		{name: "fully idle accepts cadence", mode: domain.AutoStopWhenFullyIdle, grace: 60},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &autoStopPolicyRepositoryFake{}
			service := application.NewAutoStopPolicyService(repository, &autoStopPolicyRefreshFake{}, &idsFake{values: []string{"operation-1"}}, fixedAutoStopNow)
			_, err := service.UpdateAutoStopPolicy(t.Context(), application.AutoStopPolicyUpdateInput{
				OwnerUserID: "user-1", EnvironmentID: "environment-1", PolicyID: "policy-1",
				Mode: test.mode, GracePeriod: test.grace, IdempotencyKey: "request-key-0001",
			})
			if test.wantError {
				if !errors.Is(err, application.ErrInvalidAutoStopPolicyUpdate) || repository.calls != 0 {
					t.Fatalf("UpdateAutoStopPolicy() = calls:%d error:%v", repository.calls, err)
				}
				return
			}
			if err != nil || repository.calls != 1 {
				t.Fatalf("UpdateAutoStopPolicy() = calls:%d error:%v", repository.calls, err)
			}
		})
	}
}

func TestAutoStopPolicyServiceRecordsHonestSynchronousSuccess(t *testing.T) {
	repository := &autoStopPolicyRepositoryFake{}
	refresh := &autoStopPolicyRefreshFake{}
	service := application.NewAutoStopPolicyService(repository, refresh, &idsFake{values: []string{"operation-1"}}, fixedAutoStopNow)

	update, err := service.UpdateAutoStopPolicy(t.Context(), application.AutoStopPolicyUpdateInput{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", PolicyID: "policy-1",
		Mode: domain.AutoStopWhenFullyIdle, GracePeriod: 300, IdempotencyKey: "request-key-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	operation := update.Operation().Snapshot()
	if operation.Type != domain.OperationType("environment.update_auto_stop") || operation.Status != domain.OperationSucceeded ||
		operation.RestateInvocationID != nil || operation.CompletedAt == nil || !operation.CompletedAt.Equal(fixedAutoStopNow()) {
		t.Fatalf("synchronous Operation = %#v", operation)
	}
	if refresh.calls != 1 || refresh.environmentID != "environment-1" {
		t.Fatalf("coordinator refresh = %#v", refresh)
	}
}

type autoStopPolicyRepositoryFake struct {
	policy    domain.AutoStopPolicy
	operation domain.Operation
	calls     int
}

func (repository *autoStopPolicyRepositoryFake) UpdateAutoStopPolicy(_ context.Context, _ string, policy domain.AutoStopPolicy, operation domain.Operation) (domain.Operation, bool, error) {
	repository.calls++
	repository.policy, repository.operation = policy, operation
	return operation, true, nil
}

type autoStopPolicyRefreshFake struct {
	calls         int
	environmentID string
}

func (fake *autoStopPolicyRefreshFake) DispatchAutoStopPolicyRefresh(_ context.Context, environmentID string) error {
	fake.calls++
	fake.environmentID = environmentID
	return nil
}

func fixedAutoStopNow() time.Time {
	return time.Date(2026, time.July, 19, 13, 0, 0, 0, time.UTC)
}
