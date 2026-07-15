package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrUnsupportedPolarWebhookEvent = errors.New("unsupported Polar webhook event")
	ErrMalformedPolarWebhookPayload = errors.New("malformed Polar webhook payload")
)

type PolarSubscriptionEventType string

const (
	PolarSubscriptionCreated  PolarSubscriptionEventType = "subscription.created"
	PolarSubscriptionUpdated  PolarSubscriptionEventType = "subscription.updated"
	PolarSubscriptionCanceled PolarSubscriptionEventType = "subscription.canceled"
)

type PolarSubscriptionStatus string

type PolarProjection interface {
	polarProjection()
}

type PolarSubscriptionProjection struct {
	ExternalEventID    string
	EventType          PolarSubscriptionEventType
	OccurredAt         time.Time
	SubscriptionID     string
	CustomerID         string
	ExternalCustomerID string
	Status             PolarSubscriptionStatus
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	CancelAtPeriodEnd  bool
	CanceledAt         *time.Time
}

func (PolarSubscriptionProjection) polarProjection() {}

type PolarRecurringCreditGrantProjection struct {
	ExternalEventID    string
	OccurredAt         time.Time
	GrantID            string
	SubscriptionID     string
	CustomerID         string
	ExternalCustomerID string
	MeterID            string
	Credits            int64
	CreditedAt         time.Time
}

func (PolarRecurringCreditGrantProjection) polarProjection() {}

type polarCustomerReference struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
}

func (customer polarCustomerReference) mappedExternalID(customerID string) (string, bool) {
	return customer.ExternalID, customerID != "" && customer.ID == customerID && customer.ExternalID != ""
}

func (webhook VerifiedPolarWebhook) Projection() (PolarProjection, error) {
	if webhook.externalEventID == "" || len(webhook.rawBody) == 0 {
		return nil, ErrMalformedPolarWebhookPayload
	}
	var envelope struct {
		Type      string          `json:"type"`
		Timestamp time.Time       `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(webhook.rawBody, &envelope); err != nil || envelope.Timestamp.IsZero() || len(envelope.Data) == 0 {
		return nil, ErrMalformedPolarWebhookPayload
	}
	switch PolarSubscriptionEventType(envelope.Type) {
	case PolarSubscriptionCreated, PolarSubscriptionUpdated, PolarSubscriptionCanceled:
		return projectPolarSubscription(webhook.externalEventID, PolarSubscriptionEventType(envelope.Type), envelope.Timestamp, envelope.Data)
	}
	if envelope.Type == "benefit_grant.cycled" {
		return projectPolarRecurringCreditGrant(webhook.externalEventID, envelope.Timestamp, envelope.Data)
	}
	return nil, fmt.Errorf("%w: event type is not projected", ErrUnsupportedPolarWebhookEvent)
}

func projectPolarSubscription(eventID string, eventType PolarSubscriptionEventType, occurredAt time.Time, raw json.RawMessage) (PolarProjection, error) {
	var data struct {
		ID                 string                  `json:"id"`
		Status             PolarSubscriptionStatus `json:"status"`
		CurrentPeriodStart time.Time               `json:"current_period_start"`
		CurrentPeriodEnd   time.Time               `json:"current_period_end"`
		CancelAtPeriodEnd  *bool                   `json:"cancel_at_period_end"`
		CanceledAt         *time.Time              `json:"canceled_at"`
		CustomerID         string                  `json:"customer_id"`
		Customer           polarCustomerReference  `json:"customer"`
	}
	err := json.Unmarshal(raw, &data)
	externalCustomerID, mapped := data.Customer.mappedExternalID(data.CustomerID)
	if err != nil || data.ID == "" || !validPolarSubscriptionStatus(data.Status) ||
		data.CurrentPeriodStart.IsZero() || data.CurrentPeriodEnd.IsZero() || !data.CurrentPeriodEnd.After(data.CurrentPeriodStart) ||
		data.CancelAtPeriodEnd == nil || !mapped {
		return nil, ErrMalformedPolarWebhookPayload
	}
	return PolarSubscriptionProjection{
		ExternalEventID: eventID, EventType: eventType, OccurredAt: occurredAt, SubscriptionID: data.ID,
		CustomerID: data.CustomerID, ExternalCustomerID: externalCustomerID, Status: data.Status,
		CurrentPeriodStart: data.CurrentPeriodStart, CurrentPeriodEnd: data.CurrentPeriodEnd,
		CancelAtPeriodEnd: *data.CancelAtPeriodEnd, CanceledAt: data.CanceledAt,
	}, nil
}

func projectPolarRecurringCreditGrant(eventID string, occurredAt time.Time, raw json.RawMessage) (PolarProjection, error) {
	var data struct {
		ID             string                 `json:"id"`
		IsGranted      *bool                  `json:"is_granted"`
		SubscriptionID string                 `json:"subscription_id"`
		CustomerID     string                 `json:"customer_id"`
		Customer       polarCustomerReference `json:"customer"`
		Benefit        struct {
			Type string `json:"type"`
		} `json:"benefit"`
		Properties struct {
			MeterID    string    `json:"last_credited_meter_id"`
			Credits    int64     `json:"last_credited_units"`
			CreditedAt time.Time `json:"last_credited_at"`
		} `json:"properties"`
	}
	err := json.Unmarshal(raw, &data)
	externalCustomerID, mapped := data.Customer.mappedExternalID(data.CustomerID)
	if err != nil || data.ID == "" || data.IsGranted == nil || !*data.IsGranted || data.SubscriptionID == "" || !mapped ||
		data.Benefit.Type != "meter_credit" || data.Properties.MeterID == "" || data.Properties.Credits <= 0 || data.Properties.CreditedAt.IsZero() {
		return nil, ErrMalformedPolarWebhookPayload
	}
	return PolarRecurringCreditGrantProjection{
		ExternalEventID: eventID, OccurredAt: occurredAt, GrantID: data.ID, SubscriptionID: data.SubscriptionID,
		CustomerID: data.CustomerID, ExternalCustomerID: externalCustomerID, MeterID: data.Properties.MeterID,
		Credits: data.Properties.Credits, CreditedAt: data.Properties.CreditedAt,
	}, nil
}

func validPolarSubscriptionStatus(status PolarSubscriptionStatus) bool {
	switch status {
	case "incomplete", "incomplete_expired", "trialing", "active", "past_due", "canceled", "unpaid", "paused":
		return true
	default:
		return false
	}
}
