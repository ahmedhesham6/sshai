-- +goose Up
ALTER TABLE connection_intent_idempotency
    ADD COLUMN used_at TIMESTAMPTZ,
    ADD CONSTRAINT connection_intent_used_before_expiry_check
        CHECK (used_at IS NULL OR (used_at >= created_at AND used_at < expires_at));

-- +goose Down
ALTER TABLE connection_intent_idempotency
    DROP CONSTRAINT connection_intent_used_before_expiry_check,
    DROP COLUMN used_at;
