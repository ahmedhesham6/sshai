-- +goose Up
CREATE TABLE profiles (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    name TEXT NOT NULL CHECK (name <> ''),
    slug TEXT NOT NULL CHECK (slug <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    CHECK (archived_at IS NULL OR archived_at >= created_at)
);

CREATE UNIQUE INDEX profiles_owner_slug_active_key
    ON profiles (owner_user_id, slug)
    WHERE archived_at IS NULL;

CREATE TABLE profile_versions (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    profile_id TEXT NOT NULL REFERENCES profiles (id),
    parent_version_id TEXT,
    version BIGINT NOT NULL CHECK (version > 0),
    digest TEXT NOT NULL CHECK (digest <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (parent_version_id IS NULL OR parent_version_id <> id),
    UNIQUE (profile_id, id),
    UNIQUE (profile_id, version),
    UNIQUE (profile_id, digest),
    UNIQUE (parent_version_id),
    FOREIGN KEY (profile_id, parent_version_id)
        REFERENCES profile_versions (profile_id, id)
);

CREATE TABLE environments (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    name TEXT NOT NULL CHECK (name <> ''),
    slug TEXT NOT NULL CHECK (slug <> ''),
    lifecycle TEXT NOT NULL CHECK (lifecycle IN ('creating', 'active', 'deleting', 'deleted')),
    health TEXT NOT NULL CHECK (health IN ('healthy', 'degraded', 'blocked', 'unknown')),
    region TEXT NOT NULL CHECK (region <> ''),
    availability_zone TEXT NOT NULL CHECK (availability_zone <> ''),
    runtime_preset TEXT NOT NULL CHECK (runtime_preset <> ''),
    pinned_profile_version_id TEXT NOT NULL REFERENCES profile_versions (id),
    current_runtime_id TEXT CHECK (current_runtime_id <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    version BIGINT NOT NULL CHECK (version > 0),
    CHECK (updated_at >= created_at),
    CHECK (
        (lifecycle = 'deleted' AND deleted_at = updated_at AND current_runtime_id IS NULL)
        OR (lifecycle <> 'deleted' AND deleted_at IS NULL)
    )
);

CREATE UNIQUE INDEX environments_owner_slug_active_key
    ON environments (owner_user_id, slug)
    WHERE deleted_at IS NULL;

CREATE TABLE auto_stop_policies (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    environment_id TEXT NOT NULL UNIQUE REFERENCES environments (id) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK (mode IN ('when_disconnected', 'when_agents_finish', 'when_fully_idle', 'manual')),
    grace_period_seconds INTEGER NOT NULL CHECK (grace_period_seconds BETWEEN 0 AND 86400),
    configuration JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- +goose Down
DROP TABLE auto_stop_policies;
DROP TABLE environments;
DROP TABLE profile_versions;
DROP TABLE profiles;
