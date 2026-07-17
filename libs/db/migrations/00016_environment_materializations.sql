-- +goose Up
ALTER TABLE capsule_locks
    ADD CONSTRAINT capsule_locks_id_environment_key UNIQUE (id, environment_id);

ALTER TABLE environments
    ADD COLUMN capsule_lock_id TEXT,
    ADD COLUMN upgrade_policy TEXT NOT NULL DEFAULT 'manual',
    ADD CONSTRAINT environments_capsule_lock_owner_fk
        FOREIGN KEY (capsule_lock_id, id) REFERENCES capsule_locks (id, environment_id),
    ADD CONSTRAINT environments_upgrade_policy_check CHECK (upgrade_policy IN ('manual', 'notify', 'auto_safe'));

CREATE TABLE environment_materializations (
    environment_id TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    lock_id TEXT NOT NULL,
    id TEXT CHECK (id IS NULL OR id <> ''),
    lock_digest TEXT NOT NULL CHECK (lock_digest ~ '^sha256:[a-f0-9]{64}$'),
    capsule_digest TEXT NOT NULL CHECK (capsule_digest ~ '^sha256:[a-f0-9]{64}$'),
    component_id TEXT NOT NULL CHECK (component_id <> ''),
    component_digest TEXT NOT NULL CHECK (component_digest ~ '^sha256:[a-f0-9]{64}$'),
    adapter_id TEXT NOT NULL CHECK (adapter_id <> ''),
    adapter_version TEXT NOT NULL CHECK (adapter_version <> ''),
    target_agent_version TEXT NOT NULL CHECK (target_agent_version <> ''),
    scope TEXT NOT NULL CHECK (scope IN ('user', 'project')),
    component_type TEXT NOT NULL CHECK (component_type IN ('config', 'skill', 'command', 'subagent', 'hook', 'integration', 'permission-policy', 'template', 'extension')),
    trust_class TEXT NOT NULL CHECK (trust_class IN ('declarative', 'executable', 'permission')),
    non_secret_overrides_digest TEXT CHECK (non_secret_overrides_digest IS NULL OR non_secret_overrides_digest ~ '^sha256:[a-f0-9]{64}$'),
    secret_version_identifiers JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(secret_version_identifiers) = 'array'),
    effective_cache_key TEXT NOT NULL CHECK (effective_cache_key ~ '^sha256:[a-f0-9]{64}$'),
    mode TEXT CHECK (mode IS NULL OR mode IN ('managed', 'seeded', 'referenced')),
    root TEXT CHECK (root IS NULL OR root IN ('home', 'workspace')),
    target TEXT CHECK (target IS NULL OR target <> ''),
    selector TEXT CHECK (selector IS NULL OR selector <> ''),
    directory BOOLEAN NOT NULL DEFAULT false,
    file_paths JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(file_paths) = 'array'),
    last_applied_digest TEXT CHECK (last_applied_digest IS NULL OR last_applied_digest ~ '^sha256:[a-f0-9]{64}$'),
    observed_digest TEXT CHECK (observed_digest IS NULL OR observed_digest ~ '^sha256:[a-f0-9]{64}$'),
    credential_requirement_digest TEXT CHECK (credential_requirement_digest IS NULL OR credential_requirement_digest ~ '^sha256:[a-f0-9]{64}$'),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (environment_id, component_id),
    CONSTRAINT environment_materializations_lock_owner_fk
        FOREIGN KEY (lock_id, environment_id) REFERENCES capsule_locks (id, environment_id),
    CHECK (updated_at >= created_at)
);

CREATE INDEX environment_materializations_lock_key
    ON environment_materializations (environment_id, lock_id);

-- +goose Down
DROP INDEX environment_materializations_lock_key;
DROP TABLE environment_materializations;
ALTER TABLE environments
    DROP CONSTRAINT environments_capsule_lock_owner_fk,
    DROP CONSTRAINT environments_upgrade_policy_check,
    DROP COLUMN upgrade_policy,
    DROP COLUMN capsule_lock_id;
ALTER TABLE capsule_locks
    DROP CONSTRAINT capsule_locks_id_environment_key;
