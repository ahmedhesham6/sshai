-- +goose Up
CREATE TABLE ssh_key_registrations (
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    input JSONB NOT NULL,
    ssh_key_id TEXT NOT NULL,
    PRIMARY KEY (owner_user_id, idempotency_key),
    FOREIGN KEY (owner_user_id, ssh_key_id)
        REFERENCES ssh_keys (owner_user_id, id)
);

-- +goose Down
DROP TABLE ssh_key_registrations;
