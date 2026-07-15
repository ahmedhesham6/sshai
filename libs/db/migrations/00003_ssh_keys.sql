-- +goose Up
CREATE TABLE ssh_keys (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    label TEXT NOT NULL CHECK (label <> ''),
    algorithm TEXT NOT NULL CHECK (algorithm = 'ssh-ed25519'),
    fingerprint TEXT NOT NULL CHECK (fingerprint <> ''),
    public_key TEXT NOT NULL CHECK (public_key <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    UNIQUE (owner_user_id, fingerprint),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

-- +goose Down
DROP TABLE ssh_keys;
