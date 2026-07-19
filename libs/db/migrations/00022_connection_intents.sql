-- +goose Up
CREATE TABLE connection_intent_idempotency (
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    environment_id TEXT NOT NULL,
    operation_id TEXT REFERENCES operations (id),
    intent_id TEXT NOT NULL UNIQUE CHECK (intent_id <> ''),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_user_id, idempotency_key),
    FOREIGN KEY (owner_user_id, environment_id)
        REFERENCES environments (owner_user_id, id)
);

CREATE INDEX connection_intent_idempotency_expires_at_idx
    ON connection_intent_idempotency (expires_at);

-- +goose Down
DROP TABLE connection_intent_idempotency;
