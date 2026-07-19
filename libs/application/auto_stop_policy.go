package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

var ErrInvalidAutoStopPolicyUpdate = errors.New("invalid Auto-stop Policy update")

const operationEnvironmentUpdateAutoStop domain.OperationType = "environment.update_auto_stop"

type AutoStopPolicyUpdateInput struct {
	OwnerUserID    string
	EnvironmentID  string
	PolicyID       string
	Mode           domain.AutoStopMode
	GracePeriod    int
	IdempotencyKey string
}

type AutoStopPolicyUpdate struct {
	policy    domain.AutoStopPolicy
	operation domain.Operation
}

func (update AutoStopPolicyUpdate) Policy() domain.AutoStopPolicy { return update.policy }
func (update AutoStopPolicyUpdate) Operation() domain.Operation   { return update.operation }

type AutoStopPolicyRepository interface {
	UpdateAutoStopPolicy(context.Context, string, domain.AutoStopPolicy, domain.Operation) (domain.Operation, error)
}

type AutoStopPolicyService struct {
	repository AutoStopPolicyRepository
	ids        IDGenerator
	now        func() time.Time
}

func NewAutoStopPolicyService(repository AutoStopPolicyRepository, ids IDGenerator, now func() time.Time) *AutoStopPolicyService {
	return &AutoStopPolicyService{repository: repository, ids: ids, now: now}
}

func (service *AutoStopPolicyService) UpdateAutoStopPolicy(ctx context.Context, input AutoStopPolicyUpdateInput) (AutoStopPolicyUpdate, error) {
	if service.repository == nil || service.ids == nil || service.now == nil ||
		!canonicalIdentity(input.OwnerUserID) || !canonicalIdentity(input.EnvironmentID) ||
		!canonicalIdentity(input.PolicyID) || !canonicalIdentity(input.IdempotencyKey) {
		return AutoStopPolicyUpdate{}, ErrInvalidAutoStopPolicyUpdate
	}
	policy, err := domain.NewAutoStopPolicy(input.PolicyID, input.EnvironmentID, input.Mode, input.GracePeriod)
	if err != nil {
		return AutoStopPolicyUpdate{}, fmt.Errorf("%w: %v", ErrInvalidAutoStopPolicyUpdate, err)
	}
	canonicalInput, err := json.Marshal(struct {
		GracePeriodSeconds int                 `json:"gracePeriodSeconds"`
		Mode               domain.AutoStopMode `json:"mode"`
	}{GracePeriodSeconds: input.GracePeriod, Mode: input.Mode})
	if err != nil {
		return AutoStopPolicyUpdate{}, fmt.Errorf("encode Auto-stop Policy update: %w", err)
	}
	createdAt := service.now()
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: service.ids.NewID(), EnvironmentID: input.EnvironmentID, Type: operationEnvironmentUpdateAutoStop,
		RequestedByUserID: input.OwnerUserID, IdempotencyKey: input.IdempotencyKey,
		Input: canonicalInput, CreatedAt: createdAt,
	})
	if err != nil {
		return AutoStopPolicyUpdate{}, fmt.Errorf("%w: %v", ErrInvalidAutoStopPolicyUpdate, err)
	}
	operation, err = operation.RecordRestateInvocation("control-plane:synchronous-auto-stop-policy")
	if err != nil {
		return AutoStopPolicyUpdate{}, err
	}
	operation, err = operation.Start(createdAt)
	if err != nil {
		return AutoStopPolicyUpdate{}, err
	}
	operation, err = operation.Succeed(createdAt)
	if err != nil {
		return AutoStopPolicyUpdate{}, err
	}
	persisted, err := service.repository.UpdateAutoStopPolicy(ctx, input.OwnerUserID, policy, operation)
	if err != nil {
		return AutoStopPolicyUpdate{}, fmt.Errorf("update Auto-stop Policy: %w", err)
	}
	return AutoStopPolicyUpdate{policy: policy, operation: persisted}, nil
}
