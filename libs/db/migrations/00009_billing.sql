-- +goose Up
CREATE TABLE credit_rates (
    version TEXT PRIMARY KEY CHECK (version <> ''),
    resource_type TEXT NOT NULL CHECK (resource_type IN ('compute', 'storage')),
    region TEXT NOT NULL CHECK (region <> ''),
    runtime_preset TEXT CHECK (runtime_preset <> ''),
    raw_unit TEXT NOT NULL CHECK (raw_unit <> ''),
    credits_per_unit TEXT NOT NULL CHECK (
        credits_per_unit ~ '^[0-9]+(?:\.[0-9]+)?$'
        AND credits_per_unit::NUMERIC > 0
    ),
    effective_at TIMESTAMPTZ NOT NULL,
    CHECK (
        (resource_type = 'compute' AND runtime_preset IS NOT NULL)
        OR (resource_type = 'storage' AND runtime_preset IS NULL)
    )
);

CREATE UNIQUE INDEX credit_rates_effective_key
    ON credit_rates (
        resource_type,
        region,
        COALESCE(runtime_preset, ''),
        raw_unit,
        effective_at
    );

-- +goose StatementBegin
CREATE FUNCTION reject_billing_history_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION USING
        ERRCODE = '23514',
        MESSAGE = TG_TABLE_NAME || ' is immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credit_rates_immutable
BEFORE UPDATE OR DELETE ON credit_rates
FOR EACH ROW
EXECUTE FUNCTION reject_billing_history_mutation();

CREATE TABLE compute_usage_intervals (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    user_id TEXT NOT NULL REFERENCES users (id),
    environment_id TEXT NOT NULL,
    runtime_id TEXT NOT NULL CHECK (runtime_id <> ''),
    region TEXT NOT NULL CHECK (region <> ''),
    runtime_preset TEXT NOT NULL CHECK (runtime_preset <> ''),
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    closure_source TEXT CHECK (closure_source IN ('runtime_stop', 'provider_reconciliation')),
    credit_transaction_id TEXT,
    FOREIGN KEY (user_id, environment_id)
        REFERENCES environments (owner_user_id, id),
    CHECK (ended_at IS NULL OR ended_at > started_at),
    CHECK (
        (ended_at IS NULL AND closure_source IS NULL AND credit_transaction_id IS NULL)
        OR (ended_at IS NOT NULL AND closure_source IS NOT NULL AND credit_transaction_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX compute_usage_intervals_open_environment_key
    ON compute_usage_intervals (environment_id)
    WHERE ended_at IS NULL;

CREATE UNIQUE INDEX compute_usage_intervals_open_runtime_key
    ON compute_usage_intervals (runtime_id)
    WHERE ended_at IS NULL;

-- +goose StatementBegin
CREATE FUNCTION enforce_compute_usage_interval_history()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'Compute Usage Interval history cannot be deleted';
    END IF;
    IF ROW(
        NEW.id,
        NEW.user_id,
        NEW.environment_id,
        NEW.runtime_id,
        NEW.region,
        NEW.runtime_preset,
        NEW.started_at
    ) IS DISTINCT FROM ROW(
        OLD.id,
        OLD.user_id,
        OLD.environment_id,
        OLD.runtime_id,
        OLD.region,
        OLD.runtime_preset,
        OLD.started_at
    ) OR (
        OLD.ended_at IS NOT NULL
        AND ROW(NEW.ended_at, NEW.closure_source, NEW.credit_transaction_id)
            IS DISTINCT FROM ROW(OLD.ended_at, OLD.closure_source, OLD.credit_transaction_id)
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'Compute Usage Interval history is immutable after close';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER compute_usage_intervals_history
BEFORE UPDATE OR DELETE ON compute_usage_intervals
FOR EACH ROW
EXECUTE FUNCTION enforce_compute_usage_interval_history();

CREATE TABLE credit_transactions (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    user_id TEXT NOT NULL REFERENCES users (id),
    kind TEXT NOT NULL CHECK (kind IN ('grant', 'debit', 'adjustment', 'refund')),
    credits BIGINT NOT NULL CHECK (credits <> 0),
    resource_type TEXT CHECK (resource_type IN ('compute', 'storage')),
    environment_id TEXT,
    resource_id TEXT,
    region TEXT,
    raw_quantity TEXT CHECK (raw_quantity ~ '^[0-9]+(?:\.[0-9]+)?$'),
    raw_unit TEXT,
    rate_version TEXT REFERENCES credit_rates (version),
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (user_id, environment_id)
        REFERENCES environments (owner_user_id, id),
    CHECK (created_at >= occurred_at),
    CHECK (
        (kind = 'debit' AND credits < 0
            AND resource_type IS NOT NULL
            AND environment_id IS NOT NULL
            AND resource_id IS NOT NULL AND resource_id <> ''
            AND region IS NOT NULL AND region <> ''
            AND raw_quantity IS NOT NULL
            AND raw_unit IS NOT NULL AND raw_unit <> ''
            AND rate_version IS NOT NULL)
        OR (kind IN ('grant', 'adjustment', 'refund')
            AND resource_type IS NULL
            AND environment_id IS NULL
            AND resource_id IS NULL
            AND region IS NULL
            AND raw_quantity IS NULL
            AND raw_unit IS NULL
            AND rate_version IS NULL)
    ),
    CHECK (
        (kind IN ('grant', 'refund') AND credits > 0)
        OR kind IN ('debit', 'adjustment')
    )
);

CREATE TRIGGER credit_transactions_immutable
BEFORE UPDATE OR DELETE ON credit_transactions
FOR EACH ROW
EXECUTE FUNCTION reject_billing_history_mutation();

ALTER TABLE compute_usage_intervals
    ADD CONSTRAINT compute_usage_intervals_credit_transaction_key
        UNIQUE (credit_transaction_id),
    ADD CONSTRAINT compute_usage_intervals_credit_transaction_fkey
        FOREIGN KEY (credit_transaction_id) REFERENCES credit_transactions (id);

CREATE TABLE credit_balances (
    user_id TEXT PRIMARY KEY REFERENCES users (id),
    credits BIGINT NOT NULL,
    version BIGINT NOT NULL CHECK (version >= 0),
    updated_at TIMESTAMPTZ NOT NULL
);

INSERT INTO credit_balances (user_id, credits, version, updated_at)
SELECT id, 0, 0, created_at
FROM users;

-- +goose StatementBegin
CREATE FUNCTION initialize_user_credit_balance()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO credit_balances (user_id, credits, version, updated_at)
    VALUES (NEW.id, 0, 0, NEW.created_at);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER users_initialize_credit_balance
AFTER INSERT ON users
FOR EACH ROW
EXECUTE FUNCTION initialize_user_credit_balance();

CREATE TABLE polar_deliveries (
    external_id TEXT PRIMARY KEY CHECK (external_id <> ''),
    credit_transaction_id TEXT NOT NULL UNIQUE REFERENCES credit_transactions (id),
    event_payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    delivered_at TIMESTAMPTZ,
    CHECK (event_payload ->> 'name' = 'credits_used'),
    CHECK (event_payload ->> 'external_id' = external_id),
    CHECK (delivered_at IS NULL OR delivered_at >= created_at)
);

-- +goose StatementBegin
CREATE FUNCTION enforce_polar_delivery_progress()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF ROW(
        NEW.external_id,
        NEW.credit_transaction_id,
        NEW.event_payload,
        NEW.created_at
    ) IS DISTINCT FROM ROW(
        OLD.external_id,
        OLD.credit_transaction_id,
        OLD.event_payload,
        OLD.created_at
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'PolarDelivery identity and event are immutable';
    END IF;
    IF OLD.delivered_at IS NOT NULL
        AND NEW.delivered_at IS DISTINCT FROM OLD.delivered_at THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'PolarDelivery completion is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER polar_deliveries_progress_only
BEFORE UPDATE ON polar_deliveries
FOR EACH ROW
EXECUTE FUNCTION enforce_polar_delivery_progress();

CREATE TRIGGER polar_deliveries_no_delete
BEFORE DELETE ON polar_deliveries
FOR EACH ROW
EXECUTE FUNCTION reject_billing_history_mutation();

CREATE TABLE polar_webhook_receipts (
    external_event_id TEXT PRIMARY KEY CHECK (external_event_id <> ''),
    event_type TEXT NOT NULL CHECK (event_type <> ''),
    payload_sha256 BYTEA NOT NULL CHECK (octet_length(payload_sha256) = 32),
    occurred_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL
);

CREATE TRIGGER polar_webhook_receipts_immutable
BEFORE UPDATE OR DELETE ON polar_webhook_receipts
FOR EACH ROW
EXECUTE FUNCTION reject_billing_history_mutation();

CREATE TABLE polar_customers (
    user_id TEXT PRIMARY KEY REFERENCES users (id),
    polar_customer_id TEXT NOT NULL UNIQUE CHECK (polar_customer_id <> ''),
    external_event_id TEXT NOT NULL REFERENCES polar_webhook_receipts (external_event_id),
    observed_at TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, polar_customer_id)
);

CREATE TABLE subscriptions (
    user_id TEXT PRIMARY KEY REFERENCES users (id),
    polar_subscription_id TEXT NOT NULL UNIQUE CHECK (polar_subscription_id <> ''),
    polar_customer_id TEXT NOT NULL CHECK (polar_customer_id <> ''),
    status TEXT NOT NULL CHECK (status IN (
        'incomplete', 'incomplete_expired', 'trialing', 'active',
        'past_due', 'canceled', 'unpaid', 'paused'
    )),
    current_period_start TIMESTAMPTZ NOT NULL,
    current_period_end TIMESTAMPTZ NOT NULL,
    cancel_at_period_end BOOLEAN NOT NULL,
    canceled_at TIMESTAMPTZ,
    external_event_id TEXT NOT NULL REFERENCES polar_webhook_receipts (external_event_id),
    observed_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (user_id, polar_customer_id)
        REFERENCES polar_customers (user_id, polar_customer_id),
    CHECK (current_period_end > current_period_start),
    CHECK (canceled_at IS NULL OR canceled_at >= current_period_start)
);

CREATE TABLE polar_recurring_credit_grants (
    external_event_id TEXT PRIMARY KEY REFERENCES polar_webhook_receipts (external_event_id),
    grant_id TEXT NOT NULL CHECK (grant_id <> ''),
    user_id TEXT NOT NULL REFERENCES users (id),
    polar_subscription_id TEXT NOT NULL CHECK (polar_subscription_id <> ''),
    polar_customer_id TEXT NOT NULL CHECK (polar_customer_id <> ''),
    meter_id TEXT NOT NULL CHECK (meter_id <> ''),
    credits BIGINT NOT NULL CHECK (credits > 0),
    credited_at TIMESTAMPTZ NOT NULL,
    credit_transaction_id TEXT NOT NULL UNIQUE REFERENCES credit_transactions (id),
    FOREIGN KEY (user_id, polar_customer_id)
        REFERENCES polar_customers (user_id, polar_customer_id)
);

CREATE TRIGGER polar_recurring_credit_grants_immutable
BEFORE UPDATE OR DELETE ON polar_recurring_credit_grants
FOR EACH ROW
EXECUTE FUNCTION reject_billing_history_mutation();

-- +goose Down
DROP TABLE polar_recurring_credit_grants;
DROP TABLE subscriptions;
DROP TABLE polar_customers;
DROP TABLE polar_webhook_receipts;
DROP TABLE polar_deliveries;
DROP FUNCTION enforce_polar_delivery_progress();
DROP TRIGGER users_initialize_credit_balance ON users;
DROP FUNCTION initialize_user_credit_balance();
DROP TABLE credit_balances;
DROP TABLE compute_usage_intervals;
DROP FUNCTION enforce_compute_usage_interval_history();
DROP TABLE credit_transactions;
DROP TABLE credit_rates;
DROP FUNCTION reject_billing_history_mutation();
