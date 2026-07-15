-- +goose Up
CREATE TABLE operations (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    environment_id TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN (
        'environment.create',
        'environment.delete',
        'environment.update_auto_stop',
        'runtime.start',
        'runtime.stop',
        'runtime.replace',
        'profile.apply',
        'profile.prune',
        'project.seed',
        'credential.bind',
        'environment.reconcile',
        'billing.deliver'
    )),
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'blocked')),
    requested_by_user_id TEXT NOT NULL REFERENCES users (id),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    restate_invocation_id TEXT CHECK (restate_invocation_id IS NULL OR restate_invocation_id <> ''),
    input JSONB NOT NULL,
    error_code TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    FOREIGN KEY (requested_by_user_id, environment_id)
        REFERENCES environments (owner_user_id, id),
    UNIQUE (requested_by_user_id, idempotency_key),
    CHECK (
        (status IN ('queued', 'running') AND completed_at IS NULL)
        OR (status IN ('succeeded', 'failed', 'cancelled', 'blocked') AND completed_at IS NOT NULL)
    ),
    CHECK (completed_at IS NULL OR completed_at >= created_at),
    CHECK (status <> 'succeeded' OR restate_invocation_id IS NOT NULL),
    CHECK ((error_code IS NULL) = (error_message IS NULL))
);

CREATE UNIQUE INDEX operations_one_active_per_environment_key
    ON operations (environment_id)
    WHERE status IN ('queued', 'running');

CREATE TABLE operation_steps (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    operation_id TEXT NOT NULL REFERENCES operations (id) ON DELETE CASCADE,
    step_key TEXT NOT NULL CHECK (step_key <> ''),
    status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped', 'blocked')),
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    summary TEXT NOT NULL CHECK (summary <> ''),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE (operation_id, step_key),
    CHECK (
        (status = 'pending' AND started_at IS NULL AND completed_at IS NULL)
        OR (status = 'running' AND started_at IS NOT NULL AND completed_at IS NULL)
        OR (status IN ('succeeded', 'failed', 'blocked') AND started_at IS NOT NULL AND completed_at IS NOT NULL)
        OR (status = 'skipped' AND started_at IS NULL AND completed_at IS NOT NULL)
    ),
    CHECK (completed_at IS NULL OR completed_at >= started_at)
);

-- +goose Down
DROP TABLE operation_steps;
DROP TABLE operations;
