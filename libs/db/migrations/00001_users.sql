-- +goose Up
CREATE TABLE users (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    workos_user_id TEXT NOT NULL UNIQUE CHECK (workos_user_id <> ''),
    default_region TEXT NOT NULL CHECK (default_region <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (updated_at >= created_at)
);

-- +goose Down
DROP TABLE users;
