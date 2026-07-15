-- +goose Up
ALTER TABLE environments
    ADD CONSTRAINT environments_owner_id_key UNIQUE (owner_user_id, id);

CREATE TABLE project_seeds (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    repository_url TEXT NOT NULL CHECK (repository_url <> ''),
    environment_id TEXT UNIQUE,
    base_revision TEXT NOT NULL CHECK (base_revision <> ''),
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),
    git_bundle_digest TEXT CHECK (git_bundle_digest ~ '^sha256:[a-f0-9]{64}$'),
    tracked_patch_digest TEXT CHECK (tracked_patch_digest ~ '^sha256:[a-f0-9]{64}$'),
    untracked_bundle_digest TEXT CHECK (untracked_bundle_digest ~ '^sha256:[a-f0-9]{64}$'),
    manifest_digest TEXT NOT NULL CHECK (manifest_digest ~ '^sha256:[a-f0-9]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_user_id, id),
    UNIQUE (owner_user_id, digest),
    FOREIGN KEY (owner_user_id, environment_id)
        REFERENCES environments (owner_user_id, id)
);

CREATE TABLE project_seed_registrations (
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    input JSONB NOT NULL,
    project_seed_id TEXT NOT NULL,
    PRIMARY KEY (owner_user_id, idempotency_key),
    FOREIGN KEY (owner_user_id, project_seed_id)
        REFERENCES project_seeds (owner_user_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION enforce_project_seed_immutability()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF ROW(
        NEW.id,
        NEW.owner_user_id,
        NEW.repository_url,
        NEW.base_revision,
        NEW.digest,
        NEW.git_bundle_digest,
        NEW.tracked_patch_digest,
        NEW.untracked_bundle_digest,
        NEW.manifest_digest,
        NEW.created_at
    ) IS DISTINCT FROM ROW(
        OLD.id,
        OLD.owner_user_id,
        OLD.repository_url,
        OLD.base_revision,
        OLD.digest,
        OLD.git_bundle_digest,
        OLD.tracked_patch_digest,
        OLD.untracked_bundle_digest,
        OLD.manifest_digest,
        OLD.created_at
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'Project Seed content is immutable';
    END IF;
    IF OLD.environment_id IS NOT NULL
        AND NEW.environment_id IS DISTINCT FROM OLD.environment_id THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'Project Seed cannot be reassigned';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER project_seeds_immutable
BEFORE UPDATE ON project_seeds
FOR EACH ROW
EXECUTE FUNCTION enforce_project_seed_immutability();

-- +goose Down
DROP TABLE project_seed_registrations;
DROP TABLE project_seeds;
DROP FUNCTION enforce_project_seed_immutability();
ALTER TABLE environments DROP CONSTRAINT environments_owner_id_key;
