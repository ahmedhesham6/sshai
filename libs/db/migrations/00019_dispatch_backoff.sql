-- +goose Up
ALTER TABLE workflow_outbox
    ADD COLUMN dispatch_attempts INTEGER NOT NULL DEFAULT 0 CHECK (dispatch_attempts >= 0),
    ADD COLUMN next_attempt_at TIMESTAMPTZ,
    ADD CHECK ((dispatch_attempts = 0) = (next_attempt_at IS NULL));

CREATE INDEX workflow_outbox_pending_dispatch_key
    ON workflow_outbox (dispatch_attempts, next_attempt_at, created_at, operation_id)
    WHERE started_at IS NULL;

ALTER TABLE auto_stop_policies
    ADD COLUMN refresh_attempts INTEGER NOT NULL DEFAULT 0 CHECK (refresh_attempts >= 0),
    ADD COLUMN refresh_next_attempt_at TIMESTAMPTZ,
    ADD CHECK ((refresh_attempts = 0) = (refresh_next_attempt_at IS NULL));

DROP INDEX auto_stop_policies_pending_refresh_key;
CREATE INDEX auto_stop_policies_pending_refresh_key
    ON auto_stop_policies (refresh_attempts, refresh_next_attempt_at, environment_id)
    WHERE refresh_acknowledged_generation < generation;

-- +goose Down
DROP INDEX auto_stop_policies_pending_refresh_key;
CREATE INDEX auto_stop_policies_pending_refresh_key
    ON auto_stop_policies (environment_id)
    WHERE refresh_acknowledged_generation < generation;

ALTER TABLE auto_stop_policies
    DROP COLUMN refresh_next_attempt_at,
    DROP COLUMN refresh_attempts;

DROP INDEX workflow_outbox_pending_dispatch_key;
ALTER TABLE workflow_outbox
    DROP COLUMN next_attempt_at,
    DROP COLUMN dispatch_attempts;
