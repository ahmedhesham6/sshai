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

const autoStopSnapshotCadenceSeconds = 60

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
	applied   bool
}

func (update AutoStopPolicyUpdate) Policy() domain.AutoStopPolicy { return update.policy }
func (update AutoStopPolicyUpdate) Operation() domain.Operation   { return update.operation }
func (update AutoStopPolicyUpdate) Applied() bool                 { return update.applied }

type AutoStopPolicyRepository interface {
	UpdateAutoStopPolicy(context.Context, string, domain.AutoStopPolicy, domain.Operation) (domain.Operation, bool, error)
}

type AutoStopPolicyRefreshDelivery interface {
	DispatchAutoStopPolicyRefresh(context.Context, string) error
}

type AutoStopPolicyService struct {
	repository AutoStopPolicyRepository
	refresh    AutoStopPolicyRefreshDelivery
	ids        IDGenerator
	now        func() time.Time
}

func NewAutoStopPolicyService(repository AutoStopPolicyRepository, refresh AutoStopPolicyRefreshDelivery, ids IDGenerator, now func() time.Time) *AutoStopPolicyService {
	return &AutoStopPolicyService{repository: repository, refresh: refresh, ids: ids, now: now}
}

func (service *AutoStopPolicyService) UpdateAutoStopPolicy(ctx context.Context, input AutoStopPolicyUpdateInput) (AutoStopPolicyUpdate, error) {
	if service.repository == nil || service.refresh == nil || service.ids == nil || service.now == nil ||
		!canonicalIdentity(input.OwnerUserID) || !canonicalIdentity(input.EnvironmentID) ||
		!canonicalIdentity(input.PolicyID) || !canonicalIdentity(input.IdempotencyKey) {
		return AutoStopPolicyUpdate{}, ErrInvalidAutoStopPolicyUpdate
	}
	if input.Mode != domain.AutoStopManual && input.GracePeriod < autoStopSnapshotCadenceSeconds {
		return AutoStopPolicyUpdate{}, fmt.Errorf("%w: grace period must be at least %d seconds", ErrInvalidAutoStopPolicyUpdate, autoStopSnapshotCadenceSeconds)
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
		ID: service.ids.NewID(), EnvironmentID: input.EnvironmentID, Type: domain.OperationEnvironmentUpdateAutoStop,
		RequestedByUserID: input.OwnerUserID, IdempotencyKey: input.IdempotencyKey,
		Input: canonicalInput, CreatedAt: createdAt,
	})
	if err != nil {
		return AutoStopPolicyUpdate{}, fmt.Errorf("%w: %v", ErrInvalidAutoStopPolicyUpdate, err)
	}
	// Policy persistence remains synchronous. The idempotent refresh signal
	// below durably cancels or replaces any pending coordinator timer.
	operation, err = operation.SucceedSynchronously(createdAt)
	if err != nil {
		return AutoStopPolicyUpdate{}, err
	}
	persisted, applied, err := service.repository.UpdateAutoStopPolicy(ctx, input.OwnerUserID, policy, operation)
	if err != nil {
		return AutoStopPolicyUpdate{}, fmt.Errorf("update Auto-stop Policy: %w", err)
	}
	if err := service.refresh.DispatchAutoStopPolicyRefresh(ctx, input.EnvironmentID); err != nil {
		return AutoStopPolicyUpdate{}, fmt.Errorf("update Auto-stop Policy: signal coordinator refresh: %w", err)
	}
	return AutoStopPolicyUpdate{policy: policy, operation: persisted, applied: applied}, nil
}
