package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStoreUpdatesAutoStopPolicyWithHonestSynchronousOperation(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 19, 13, 0, 0, 0, time.UTC)
	insertRuntimeOperationState(t, ctx, pool, createdAt.Add(-time.Hour))
	policy := autoStopPolicy(t, domain.AutoStopWhenFullyIdle, 300)

	tests := []struct {
		name        string
		operationID string
		wantApplied bool
	}{
		{name: "apply", operationID: "operation-policy-1", wantApplied: true},
		{name: "replay", operationID: "operation-unused", wantApplied: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := synchronousPolicyOperation(t, test.operationID, "request-policy-0001", createdAt)
			operation, applied, err := store.UpdateAutoStopPolicy(ctx, "user-1", policy, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if applied != test.wantApplied || operation.Snapshot().ID != "operation-policy-1" || operation.Snapshot().RestateInvocationID != nil {
				t.Fatalf("UpdateAutoStopPolicy() = Operation:%#v applied:%t", operation.Snapshot(), applied)
			}
		})
	}

	var mode string
	var grace int
	var invocationID *string
	if err := pool.QueryRow(ctx, `
		SELECT policy.mode, policy.grace_period_seconds, operation.restate_invocation_id
		FROM auto_stop_policies policy
		JOIN operations operation ON operation.environment_id = policy.environment_id
		WHERE policy.id = 'policy-1' AND operation.id = 'operation-policy-1'`).Scan(&mode, &grace, &invocationID); err != nil {
		t.Fatal(err)
	}
	if mode != string(domain.AutoStopWhenFullyIdle) || grace != 300 || invocationID != nil {
		t.Fatalf("stored Policy/Operation = mode:%q grace:%d invocation:%v", mode, grace, invocationID)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO operations (
			id, environment_id, type, status, requested_by_user_id, idempotency_key,
			input, created_at, completed_at
		) VALUES (
			'operation-workflow-without-invocation', 'environment-1', 'runtime.start', 'succeeded',
			'user-1', 'request-workflow-0001', '{}'::jsonb, $1, $1
		)`, createdAt)
	assertPostgreSQLCode(t, err, "23514", "workflow success without Restate invocation")
}

func TestStoreSerializesRuntimeAndPolicyCommandsBySharedIdempotencyKey(t *testing.T) {
	tests := []struct {
		name          string
		operationType domain.OperationType
		input         []byte
	}{
		{name: "Runtime start", operationType: domain.OperationRuntimeStart, input: []byte(`{}`)},
		{name: "Runtime stop", operationType: domain.OperationRuntimeStop, input: []byte(`{"reason":"manual"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, pool := openTestStoreAndPool(t, ctx)
			createdAt := time.Date(2026, time.July, 19, 13, 0, 0, 0, time.UTC)
			insertRuntimeOperationState(t, ctx, pool, createdAt.Add(-time.Hour))
			idempotencyKey := "shared-request-key-0001"
			policy := autoStopPolicy(t, domain.AutoStopWhenFullyIdle, 300)
			policyOperation := synchronousPolicyOperation(t, "operation-policy", idempotencyKey, createdAt)
			runtimeOperation := runtimeOperationCandidate(t, "operation-runtime", "environment-1", test.operationType, idempotencyKey, test.input, createdAt)

			start := make(chan struct{})
			results := make(chan error, 2)
			go func() {
				<-start
				_, _, err := store.UpdateAutoStopPolicy(ctx, "user-1", policy, policyOperation)
				results <- err
			}()
			go func() {
				<-start
				_, err := store.ReserveRuntimeOperation(ctx, runtimeOperation)
				results <- err
			}()
			close(start)
			first, second := <-results, <-results
			if !((first == nil && errors.Is(second, dbstore.ErrIdempotencyConflict)) ||
				(second == nil && errors.Is(first, dbstore.ErrIdempotencyConflict))) {
				t.Fatalf("concurrent commands = %v, %v; want one success and one idempotency conflict", first, second)
			}
		})
	}
}

func autoStopPolicy(t *testing.T, mode domain.AutoStopMode, grace int) domain.AutoStopPolicy {
	t.Helper()
	policy, err := domain.NewAutoStopPolicy("policy-1", "environment-1", mode, grace)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func synchronousPolicyOperation(t *testing.T, operationID, idempotencyKey string, createdAt time.Time) domain.Operation {
	t.Helper()
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: operationID, EnvironmentID: "environment-1", Type: domain.OperationEnvironmentUpdateAutoStop,
		RequestedByUserID: "user-1", IdempotencyKey: idempotencyKey,
		Input: []byte(`{"gracePeriodSeconds":300,"mode":"when_fully_idle"}`), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err = operation.SucceedSynchronously(createdAt)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}
