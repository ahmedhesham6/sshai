-- +goose Up
ALTER TABLE operations DROP CONSTRAINT operations_type_check;
ALTER TABLE operations ADD CONSTRAINT operations_type_check CHECK (type IN (
    'environment.create',
    'environment.delete',
    'environment.update_auto_stop',
    'runtime.start',
    'runtime.stop',
    'runtime.replace',
    'profile.apply',
    'profile.prune',
    'profile.resolve',
    'project.seed',
    'credential.bind',
    'environment.reconcile',
    'billing.deliver'
));

CREATE TABLE capsule_locks (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    environment_id TEXT NOT NULL REFERENCES environments (id),
    profile_version_id TEXT NOT NULL REFERENCES profile_versions (id),
    project_capsule_digest TEXT NOT NULL CHECK (project_capsule_digest ~ '^sha256:[a-f0-9]{64}$'),
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),
    capsules JSONB NOT NULL CHECK (jsonb_typeof(capsules) = 'array'),
    resolved_components JSONB NOT NULL CHECK (jsonb_typeof(resolved_components) = 'object'),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT capsule_locks_target_key UNIQUE (environment_id, profile_version_id, project_capsule_digest),
    CONSTRAINT capsule_locks_digest_key UNIQUE (digest)
);

-- +goose StatementBegin
CREATE FUNCTION reject_immutable_capsule_lock() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE check_violation USING MESSAGE = 'Capsule Locks are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER capsule_locks_immutable
BEFORE UPDATE OR DELETE ON capsule_locks
FOR EACH ROW EXECUTE FUNCTION reject_immutable_capsule_lock();

-- +goose Down
DROP TRIGGER capsule_locks_immutable ON capsule_locks;
DROP FUNCTION reject_immutable_capsule_lock;
DROP TABLE capsule_locks;
ALTER TABLE operations DROP CONSTRAINT operations_type_check;
ALTER TABLE operations ADD CONSTRAINT operations_type_check CHECK (type IN (
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
));
