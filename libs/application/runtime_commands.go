package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db"
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
	ReplayRuntimeOperation(context.Context, domain.Operation) (domain.EnvironmentRuntimeOperation, bool, error)
	ReserveRuntimeOperation(context.Context, domain.Operation) (domain.EnvironmentRuntimeOperation, error)
}

type RuntimeOperationDispatcher interface {
	DispatchRuntimeOperation(context.Context, string) error
}

type RuntimeCreditBalanceSource interface {
	CreditBalance(context.Context, string) (db.CreditBalanceProjection, error)
}

type RuntimeCommandService struct {
	repository RuntimeOperationRepository
	dispatcher RuntimeOperationDispatcher
	credits    RuntimeCreditBalanceSource
	ids        IDGenerator
	now        func() time.Time
}

func NewRuntimeCommandService(repository RuntimeOperationRepository, dispatcher RuntimeOperationDispatcher, credits RuntimeCreditBalanceSource, ids IDGenerator, now func() time.Time) *RuntimeCommandService {
	return &RuntimeCommandService{repository: repository, dispatcher: dispatcher, credits: credits, ids: ids, now: now}
}

func (service *RuntimeCommandService) StartRuntime(ctx context.Context, input RuntimeCommandInput) (domain.EnvironmentRuntimeOperation, error) {
	if !service.valid(input) || service.credits == nil {
		return domain.EnvironmentRuntimeOperation{}, ErrInvalidRuntimeCommand
	}
	operation, err := service.prepareRuntimeOperation(input, domain.OperationRuntimeStart, []byte(`{}`))
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	replayed, present, err := service.repository.ReplayRuntimeOperation(ctx, operation)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("start Runtime: replay Operation: %w", err)
	}
	if present {
		return service.dispatchRuntimeOperation(ctx, replayed)
	}
	// Credit Balance is shared across a User's Environments, while Runtime
	// reservation locks only one Environment. Concurrent starts may therefore
	// both observe a positive balance. The private-alpha policy in spec 10
	// explicitly accepts slight negative balances, so this admission race is
	// intentional until paid-launch enforcement introduces User-wide admission.
	balance, err := service.credits.CreditBalance(ctx, input.OwnerUserID)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("start Runtime: load Credit Balance: %w", err)
	}
	if balance.UserID != input.OwnerUserID {
		return domain.EnvironmentRuntimeOperation{}, errors.New("start Runtime: Credit Balance belongs to another User")
	}
	if balance.Credits <= 0 {
		return domain.EnvironmentRuntimeOperation{}, ErrCreditsPolicyBlocked
	}
	return service.reserveRuntimeOperation(ctx, operation)
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
	if !service.valid(input) {
		return domain.EnvironmentRuntimeOperation{}, ErrInvalidRuntimeCommand
	}
	operation, err := service.prepareRuntimeOperation(input, operationType, canonicalInput)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, err
	}
	return service.reserveRuntimeOperation(ctx, operation)
}

func (service *RuntimeCommandService) prepareRuntimeOperation(input RuntimeCommandInput, operationType domain.OperationType, canonicalInput []byte) (domain.Operation, error) {
	return domain.QueueOperation(domain.OperationRequest{
		ID: service.ids.NewID(), EnvironmentID: input.EnvironmentID, Type: operationType,
		RequestedByUserID: input.OwnerUserID, IdempotencyKey: input.IdempotencyKey,
		Input: canonicalInput, CreatedAt: service.now(),
	})
}

func (service *RuntimeCommandService) reserveRuntimeOperation(ctx context.Context, operation domain.Operation) (domain.EnvironmentRuntimeOperation, error) {
	command, err := service.repository.ReserveRuntimeOperation(ctx, operation)
	if err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("command Runtime: reserve Operation: %w", err)
	}
	return service.dispatchRuntimeOperation(ctx, command)
}

func (service *RuntimeCommandService) dispatchRuntimeOperation(ctx context.Context, command domain.EnvironmentRuntimeOperation) (domain.EnvironmentRuntimeOperation, error) {
	if err := service.dispatcher.DispatchRuntimeOperation(ctx, command.Operation().Snapshot().ID); err != nil {
		return domain.EnvironmentRuntimeOperation{}, fmt.Errorf("command Runtime: dispatch Operation: %w", err)
	}
	return command, nil
}

func (service *RuntimeCommandService) valid(input RuntimeCommandInput) bool {
	return service != nil && service.repository != nil && service.dispatcher != nil && service.ids != nil && service.now != nil &&
		canonicalIdentity(input.OwnerUserID) && canonicalIdentity(input.EnvironmentID) && canonicalIdentity(input.IdempotencyKey)
}
