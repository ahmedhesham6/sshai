-- +goose Up
CREATE TABLE capsule_tags (
    owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    tag TEXT NOT NULL CHECK (tag ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_user_id, name, tag)
);

-- +goose Down
DROP TABLE capsule_tags;
