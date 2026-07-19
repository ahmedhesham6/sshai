package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

var (
	ErrInvalidRuntimeCommand = errors.New("invalid Runtime command")
	ErrCreditsPolicyBlocked  = errors.New("credit policy blocks Runtime start")
)

type RuntimeCommandInput struct {
	OwnerUserID    string
	EnvironmentID  string
	IdempotencyKey string
}

type RuntimeOperationRepository interface {
	ReserveRuntimeOperation(context.Context, domain.Operation) (domain.EnvironmentRuntimeOperation, error)
}

type RuntimeOperationDispatcher interface {
	DispatchRuntimeOperation(context.Context, string) error
}

type RuntimeCommandService struct {
	repository RuntimeOperationRepository
	dispatcher RuntimeOperationDispatcher
	ids        IDGenerator
	now        func() time.Time
}

func NewRuntimeCommandService(repository RuntimeOperationRepository, dispatcher RuntimeOperationDispatcher, ids IDGenerator, now func() time.Time) *RuntimeCommandService {
	return &RuntimeCommandService{repository: repository, dispatcher: dispatcher, ids: ids, now: now}
}

func (service *RuntimeCommandService) StartRuntime(ctx context.Context, input RuntimeCommandInput) (domain.EnvironmentRuntimeOperation, error) {
	return service.commandRuntime(ctx, input, domain.OperationRuntimeStart, []byte(`{}`))
}

func (service *RuntimeCommandService) StopRuntime(ctx context.Context, input RuntimeCommandInput) (domain.EnvironmentRuntimeOperation, error) {
	return service.StopRuntimeWithReason(ctx, input, domain.RuntimeStopManual, nil)
}

func (service *RuntimeCommandService) StopRuntimeWithReason(ctx context.Context, input RuntimeCommandInput, reason domain.RuntimeStopReason, audit *domain.RuntimeStopAuditEvidence) (domain.EnvironmentRuntimeOperation, error) {
	if !reason.Valid() || reason == domain.RuntimeStopAutoStop && audit == nil || reason != domain.RuntimeStopAutoStop && audit != nil {
		return domain.EnvironmentRuntimeOperation{}, ErrInvalidRuntimeCommand
	}
	canonicalInput, err := json.Marshal(struct {
		Reason domain.RuntimeStopReason         `json:"reason"`
		Audit  *domain.RuntimeStopAuditEvidence `json:"audit,omitempty"`
	}{Reason: reason, Audit: domain.CloneRuntimeStopAuditEvidence(audit)})
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	return service.commandRuntime(ctx, input, domain.OperationRuntimeStop, canonicalInput)
}

func (service *RuntimeCommandService) ReplaceRuntime(ctx context.Context, input RuntimeCommandInput) (domain.EnvironmentRuntimeOperation, error) {
	return service.commandRuntime(ctx, input, domain.OperationRuntimeReplace, []byte(`{}`))
}

func (service *RuntimeCommandService) commandRuntime(ctx context.Context, input RuntimeCommandInput, operationType domain.OperationType, canonicalInput []byte) (domain.EnvironmentRuntimeOperation, error) {
	if service.repository == nil || service.dispatcher == nil || service.ids == nil || service.now == nil ||
		!canonicalIdentity(input.OwnerUserID) || !canonicalIdentity(input.EnvironmentID) || !canonicalIdentity(input.IdempotencyKey) {
		return domain.EnvironmentRuntimeOperation{}, ErrInvalidRuntimeCommand
	}
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID: service.ids.NewID(), EnvironmentID: input.EnvironmentID, Type: operationType,
		RequestedByUserID: input.OwnerUserID, IdempotencyKey: input.IdempotencyKey,
		Input: canonicalInput, CreatedAt: service.now(),
	})
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	command, err := service.repository.ReserveRuntimeOperation(ctx, operation)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("command Runtime: reserve Operation: %w", err)
	}
	if err := service.dispatcher.DispatchRuntimeOperation(ctx, command.Operation().Snapshot().ID); err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("command Runtime: dispatch Operation: %w", err)
	}
	return command, nil
}
