# Billing and Polar credits

## Product model

A Polar subscription grants one shared pool of abstract credits per billing period. All billable resource types convert measured usage to that unit and debit the same balance.

```text
subscription renewal
  → credit grant

runtime active duration × regional Runtime Preset rate
  → compute credit debit

persistent GiB × elapsed time × regional storage rate
  → storage credit debit
```

There are no separate compute and storage wallets.

## Polar model

- One subscription product.
- One usage meter, for example `credits_used`.
- A recurring meter-credit benefit grants the plan allowance.
- Usage events sum a `credits` metadata property.
- Metadata includes `resource_type`, `environment_id`, `region`, `raw_quantity`, `raw_unit`, and `rate_version`.
- Polar is authoritative for checkout, payment collection, subscription status, invoices, and the external meter.
- PostgreSQL is authoritative for measured raw usage, conversion, the internal Credit Transaction ledger, and delivery state.

## Credit-rate table

Rates are versioned product configuration:

```go
type CreditRate struct {
	Version      string
	ResourceType string
	Region       string
	Preset       *string
	RawUnit      string
	CreditsPerUnit decimal.Decimal
	EffectiveAt  time.Time
}
```

A Credit Transaction stores the exact rate version. Rate changes never rewrite historical debits.

## Compute measurement

- Open the interval when AWS begins billable Runtime start semantics, not when the first SSH client connects.
- Close it when provider state reaches stopped or terminated.
- Reconciliation closes orphaned intervals from observed provider state.
- Convert duration through the Runtime Preset and regional rate.
- Apply AWS minimum billing behavior inside the rate/conversion policy when necessary.
- One compute interval produces one idempotent debit and one Polar event.

## Storage measurement

- Persistent data volume allocation starts storage usage.
- Usage continues while the Runtime is stopped.
- Measure allocated GiB, not filesystem used bytes, because provider cost is capacity-based.
- Emit bounded periodic debits or one debit per billing aggregation window.
- Deletion closes the final storage window.

## Ledger and delivery

1. Measure raw usage.
2. In one PostgreSQL transaction, append the debit and update the Credit Balance projection.
3. Create a `PolarDelivery` outbox row with the same stable idempotency key.
4. Invoke a Restate billing-delivery workflow.
5. Send the immutable event to Polar.
6. Record the Polar event identifier and delivery completion.
7. Reconcile local consumption with Polar customer-meter state.

Client-side usage is never trusted. Retrying delivery must not create duplicate credit consumption.

## Webhooks

Verify Polar webhook signatures and store the external event ID before processing. Project:

- subscription created/updated/cancelled;
- order/payment state;
- recurring credit grants;
- customer identity mapping;
- external meter state needed for reconciliation.

Webhook replay is idempotent.

## UX

Show:

- current Credit Balance;
- subscription renewal date;
- compute and storage consumption in credits;
- raw usage behind an expandable breakdown;
- projected remaining time for the selected Runtime Preset;
- a link to the Polar customer portal.

The zero-credit enforcement policy remains intentionally open and must be resolved before paid launch.
