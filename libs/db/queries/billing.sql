-- name: InsertCreditRate :execrows
INSERT INTO credit_rates (
    version, resource_type, region, runtime_preset, raw_unit,
    credits_per_unit, effective_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (version) DO NOTHING;

-- name: GetCreditRate :one
SELECT version, resource_type, region, runtime_preset, raw_unit,
       credits_per_unit, effective_at
FROM credit_rates
WHERE version = $1;

-- name: GetOwnedEnvironmentRuntimeForUpdate :one
SELECT region, runtime_preset, current_runtime_id
FROM environments
WHERE id = $1 AND owner_user_id = $2
FOR UPDATE;

-- name: InsertComputeUsageInterval :execrows
INSERT INTO compute_usage_intervals (
    id, user_id, environment_id, runtime_id, region, runtime_preset, started_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO NOTHING;

-- name: GetComputeUsageInterval :one
SELECT id, user_id, environment_id, runtime_id, region, runtime_preset,
       started_at, ended_at, closure_source, credit_transaction_id
FROM compute_usage_intervals
WHERE id = $1;

-- name: GetComputeUsageIntervalForUpdate :one
SELECT id, user_id, environment_id, runtime_id, region, runtime_preset,
       started_at, ended_at, closure_source, credit_transaction_id
FROM compute_usage_intervals
WHERE id = $1
FOR UPDATE;

-- name: GetApplicableComputeCreditRate :one
SELECT version, resource_type, region, runtime_preset, raw_unit,
       credits_per_unit, effective_at
FROM credit_rates
WHERE resource_type = 'compute'
  AND region = $1
  AND runtime_preset = $2
  AND raw_unit = 'second'
  AND effective_at <= $3
ORDER BY effective_at DESC, version DESC
LIMIT 1;

-- name: InsertCreditTransaction :exec
INSERT INTO credit_transactions (
    id, user_id, kind, credits, resource_type, environment_id, resource_id,
    region, raw_quantity, raw_unit, rate_version, idempotency_key,
    occurred_at, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12, $13, $14
);

-- name: GetCreditTransaction :one
SELECT id, user_id, kind, credits, resource_type, environment_id, resource_id,
       region, raw_quantity, raw_unit, rate_version, idempotency_key,
       occurred_at, created_at
FROM credit_transactions
WHERE id = $1;

-- name: ApplyCreditBalanceTransaction :one
INSERT INTO credit_balances (user_id, credits, version, updated_at)
VALUES ($1, $2, 1, $3)
ON CONFLICT (user_id) DO UPDATE
SET credits = credit_balances.credits + EXCLUDED.credits,
    version = credit_balances.version + 1,
    updated_at = EXCLUDED.updated_at
RETURNING user_id, credits, version, updated_at;

-- name: GetCreditBalance :one
SELECT user_id, credits, version, updated_at
FROM credit_balances
WHERE user_id = $1;

-- name: InsertPolarDelivery :exec
INSERT INTO polar_deliveries (
    external_id, credit_transaction_id, event_payload, created_at
) VALUES ($1, $2, $3, $4);

-- name: GetPolarDelivery :one
SELECT external_id, credit_transaction_id, event_payload, created_at, delivered_at
FROM polar_deliveries
WHERE external_id = $1;

-- name: CompletePolarDelivery :one
UPDATE polar_deliveries
SET delivered_at = COALESCE(delivered_at, $2)
WHERE external_id = $1
RETURNING external_id, credit_transaction_id, event_payload, created_at, delivered_at;

-- name: CloseComputeUsageInterval :execrows
UPDATE compute_usage_intervals
SET ended_at = $2, closure_source = $3, credit_transaction_id = $4
WHERE id = $1 AND ended_at IS NULL;

-- name: LockPolarWebhookReceipt :one
SELECT pg_advisory_xact_lock(hashtextextended($1, 0));

-- name: GetPolarWebhookReceipt :one
SELECT external_event_id, event_type, payload_sha256, occurred_at, received_at
FROM polar_webhook_receipts
WHERE external_event_id = $1;

-- name: InsertPolarWebhookReceipt :exec
INSERT INTO polar_webhook_receipts (
    external_event_id, event_type, payload_sha256, occurred_at, received_at
) VALUES ($1, $2, $3, $4, $5);

-- name: InsertPolarCustomer :execrows
INSERT INTO polar_customers (
    user_id, polar_customer_id, external_event_id, observed_at
) VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO NOTHING;

-- name: GetPolarCustomer :one
SELECT user_id, polar_customer_id, external_event_id, observed_at
FROM polar_customers
WHERE user_id = $1;

-- name: UpsertSubscription :exec
INSERT INTO subscriptions (
    user_id, polar_subscription_id, polar_customer_id, status,
    current_period_start, current_period_end, cancel_at_period_end,
    canceled_at, external_event_id, observed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (user_id) DO UPDATE
SET polar_subscription_id = EXCLUDED.polar_subscription_id,
    polar_customer_id = EXCLUDED.polar_customer_id,
    status = EXCLUDED.status,
    current_period_start = EXCLUDED.current_period_start,
    current_period_end = EXCLUDED.current_period_end,
    cancel_at_period_end = EXCLUDED.cancel_at_period_end,
    canceled_at = EXCLUDED.canceled_at,
    external_event_id = EXCLUDED.external_event_id,
    observed_at = EXCLUDED.observed_at
WHERE subscriptions.observed_at <= EXCLUDED.observed_at;

-- name: GetSubscription :one
SELECT user_id, polar_subscription_id, polar_customer_id, status,
       current_period_start, current_period_end, cancel_at_period_end,
       canceled_at, external_event_id, observed_at
FROM subscriptions
WHERE user_id = $1;

-- name: InsertPolarRecurringCreditGrant :exec
INSERT INTO polar_recurring_credit_grants (
    external_event_id, grant_id, user_id, polar_subscription_id,
    polar_customer_id, meter_id, credits, credited_at, credit_transaction_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
