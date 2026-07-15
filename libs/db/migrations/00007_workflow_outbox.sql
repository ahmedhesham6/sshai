-- +goose Up
CREATE TABLE workflow_outbox (
    operation_id TEXT PRIMARY KEY REFERENCES operations (id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind = 'environment.create'),
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    restate_invocation_id TEXT CHECK (restate_invocation_id IS NULL OR restate_invocation_id <> ''),
    CHECK ((started_at IS NULL) = (restate_invocation_id IS NULL)),
    CHECK (started_at IS NULL OR started_at >= created_at)
);

-- +goose Down
DROP TABLE workflow_outbox;
