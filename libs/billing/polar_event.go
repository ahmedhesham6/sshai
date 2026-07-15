package billing

import (
	"encoding/json"
	"errors"
	"time"
)

const creditsUsedEventName = "credits_used"

type CreditsUsedEvent struct {
	externalCustomerID string
	externalID         string
	timestamp          time.Time
	metadata           creditsUsedMetadata
}

type creditsUsedMetadata struct {
	Credits       int64        `json:"credits"`
	ResourceType  ResourceType `json:"resource_type"`
	EnvironmentID string       `json:"environment_id"`
	Region        string       `json:"region"`
	RawQuantity   string       `json:"raw_quantity"`
	RawUnit       string       `json:"raw_unit"`
	RateVersion   string       `json:"rate_version"`
}

func NewCreditsUsedEvent(transaction CreditTransaction) (CreditsUsedEvent, error) {
	usage, debit := transaction.DebitUsage()
	if !debit {
		return CreditsUsedEvent{}, errors.New("create Polar credits_used event: approved debit is required")
	}
	return CreditsUsedEvent{
		externalCustomerID: transaction.UserID(),
		externalID:         transaction.IdempotencyKey(),
		timestamp:          transaction.OccurredAt(),
		metadata: creditsUsedMetadata{
			Credits: -transaction.Credits(), ResourceType: usage.ResourceType, EnvironmentID: usage.EnvironmentID,
			Region: usage.Region, RawQuantity: usage.RawQuantity, RawUnit: usage.RawUnit, RateVersion: usage.RateVersion,
		},
	}, nil
}

func (event CreditsUsedEvent) ExternalID() string { return event.externalID }

func (event CreditsUsedEvent) MarshalJSON() ([]byte, error) {
	if event.externalCustomerID == "" || event.externalID == "" || event.timestamp.IsZero() || event.metadata.Credits <= 0 {
		return nil, errors.New("marshal Polar credits_used event: event is not initialized")
	}
	return json.Marshal(struct {
		Name               string              `json:"name"`
		ExternalCustomerID string              `json:"external_customer_id"`
		Timestamp          time.Time           `json:"timestamp"`
		ExternalID         string              `json:"external_id"`
		Metadata           creditsUsedMetadata `json:"metadata"`
	}{creditsUsedEventName, event.externalCustomerID, event.timestamp, event.externalID, event.metadata})
}
