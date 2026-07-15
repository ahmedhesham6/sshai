package workflows

import (
	"context"
	"errors"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

const BillingDeliveryService = "BillingDelivery"

type BillingDeliveryInput struct {
	ExternalID string `json:"externalId"`
}

type BillingDeliveryOutput struct {
	ExternalID string `json:"externalId"`
	Delivered  bool   `json:"delivered"`
}

type PolarEventDeliverer interface {
	Deliver(context.Context, billing.CreditsUsedEvent) error
}

type PolarDeliveryStore interface {
	PolarDeliveryEvent(context.Context, string) (billing.CreditsUsedEvent, bool, bool, error)
	RecordPolarDeliverySuccess(context.Context, string, time.Time) error
}

type billingDeliveryWorkflow struct {
	client PolarEventDeliverer
	store  PolarDeliveryStore
	now    func() time.Time
}

func BillingDeliveryDefinition(client PolarEventDeliverer, store PolarDeliveryStore, now func() time.Time) restate.ServiceDefinition {
	workflow := &billingDeliveryWorkflow{client: client, store: store, now: now}
	return restate.NewWorkflow(BillingDeliveryService).Handler(
		RunHandler,
		restate.NewWorkflowHandler(workflow.Run),
	)
}

func (workflow *billingDeliveryWorkflow) Run(ctx restate.WorkflowContext, input BillingDeliveryInput) (BillingDeliveryOutput, error) {
	if input.ExternalID == "" || restate.Key(ctx) != input.ExternalID {
		return BillingDeliveryOutput{}, restate.TerminalErrorf("workflow key does not match PolarDelivery external ID")
	}
	deliveredNow, err := restate.Run(ctx, func(runCtx restate.RunContext) (bool, error) {
		return deliverPendingPolarEvent(runCtx, workflow.client, workflow.store, input.ExternalID)
	}, restate.WithName("deliver-polar-event"))
	if err != nil {
		return BillingDeliveryOutput{}, err
	}
	if deliveredNow {
		if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
			return workflow.store.RecordPolarDeliverySuccess(runCtx, input.ExternalID, workflow.now())
		}, restate.WithName("complete-polar-delivery")); err != nil {
			return BillingDeliveryOutput{}, err
		}
	}
	return BillingDeliveryOutput{ExternalID: input.ExternalID, Delivered: true}, nil
}

func deliverPendingPolarEvent(
	ctx context.Context,
	client PolarEventDeliverer,
	store PolarDeliveryStore,
	externalID string,
) (bool, error) {
	event, delivered, found, err := store.PolarDeliveryEvent(ctx, externalID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, restate.TerminalErrorf("PolarDelivery does not exist")
	}
	if delivered {
		return false, nil
	}
	if err := client.Deliver(ctx, event); err != nil {
		var terminal *billing.PolarTerminalError
		if errors.As(err, &terminal) {
			return false, restate.ToTerminalError(err)
		}
		return false, err
	}
	return true, nil
}

func (client *Client) SendBillingDelivery(ctx context.Context, input BillingDeliveryInput) error {
	_, err := ingress.WorkflowSend[BillingDeliveryInput](
		client.ingress, BillingDeliveryService, input.ExternalID, RunHandler,
	).Send(ctx, input)
	return err
}
