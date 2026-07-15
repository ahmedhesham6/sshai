-- +goose Up
ALTER TABLE profiles
    ADD CONSTRAINT profiles_owner_id_key UNIQUE (owner_user_id, id);

ALTER TABLE profile_versions
    ADD CONSTRAINT profile_versions_digest_sha256_check
        CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),
    ADD CONSTRAINT profile_versions_parent_shape_check
        CHECK ((version = 1) = (parent_version_id IS NULL));

-- +goose StatementBegin
CREATE FUNCTION enforce_profile_version_linearity() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    parent_profile_id TEXT;
    parent_version BIGINT;
BEGIN
    IF NEW.parent_version_id IS NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.parent_version_id = NEW.id THEN
        RAISE check_violation USING MESSAGE = 'Profile Version cannot parent itself';
    END IF;
    SELECT profile_id, version
      INTO parent_profile_id, parent_version
      FROM profile_versions
     WHERE id = NEW.parent_version_id;
    IF NOT FOUND OR parent_profile_id <> NEW.profile_id THEN
        RAISE foreign_key_violation USING MESSAGE = 'Profile Version parent belongs to another Profile';
    END IF;
    IF parent_version <> NEW.version - 1 THEN
        RAISE check_violation USING MESSAGE = 'Profile Version parent must be the immediately preceding version';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER profile_versions_linear_insert
BEFORE INSERT ON profile_versions
FOR EACH ROW EXECUTE FUNCTION enforce_profile_version_linearity();

CREATE TABLE profile_artifacts (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    profile_version_id TEXT NOT NULL REFERENCES profile_versions (id),
    kind TEXT NOT NULL CHECK (kind IN (
        'agent_instruction', 'codex_settings', 'claude_settings', 'shell_preferences',
        'git_preferences', 'agent_skill_instruction', 'agent_skill_executable'
    )),
    source_locator TEXT NOT NULL CHECK (source_locator <> ''),
    source_digest TEXT NOT NULL CHECK (source_digest ~ '^sha256:[a-f0-9]{64}$'),
    content_digest TEXT NOT NULL CHECK (content_digest ~ '^sha256:[a-f0-9]{64}$'),
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    mode INTEGER NOT NULL CHECK (mode BETWEEN 0 AND 511),
    sensitivity TEXT NOT NULL CHECK (sensitivity IN ('public', 'private')),
    trust TEXT NOT NULL CHECK (trust IN ('user_authored', 'trusted_source', 'third_party')),
    contains_executable BOOLEAN NOT NULL,
    UNIQUE (profile_version_id, source_locator),
    CHECK (contains_executable = (kind IN ('shell_preferences', 'agent_skill_executable')))
);

-- +goose StatementBegin
CREATE FUNCTION reject_immutable_profile_publication() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE check_violation USING MESSAGE = 'Profile Versions and Profile Artifacts are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER profile_versions_immutable
BEFORE UPDATE OR DELETE ON profile_versions
FOR EACH ROW EXECUTE FUNCTION reject_immutable_profile_publication();

CREATE TRIGGER profile_artifacts_immutable
BEFORE UPDATE OR DELETE ON profile_artifacts
FOR EACH ROW EXECUTE FUNCTION reject_immutable_profile_publication();

CREATE TABLE profile_create_registrations (
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    input JSONB NOT NULL,
    profile_id TEXT NOT NULL,
    PRIMARY KEY (owner_user_id, idempotency_key),
    FOREIGN KEY (owner_user_id, profile_id) REFERENCES profiles (owner_user_id, id)
);

CREATE TABLE profile_publication_registrations (
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    input JSONB NOT NULL,
    profile_id TEXT NOT NULL,
    profile_version_id TEXT NOT NULL,
    PRIMARY KEY (owner_user_id, idempotency_key),
    FOREIGN KEY (owner_user_id, profile_id) REFERENCES profiles (owner_user_id, id),
    FOREIGN KEY (profile_id, profile_version_id) REFERENCES profile_versions (profile_id, id)
);

-- +goose Down
DROP TABLE profile_publication_registrations;
DROP TABLE profile_create_registrations;
DROP TRIGGER profile_artifacts_immutable ON profile_artifacts;
DROP TRIGGER profile_versions_immutable ON profile_versions;
DROP FUNCTION reject_immutable_profile_publication;
DROP TABLE profile_artifacts;
DROP TRIGGER profile_versions_linear_insert ON profile_versions;
DROP FUNCTION enforce_profile_version_linearity;
ALTER TABLE profile_versions
    DROP CONSTRAINT profile_versions_parent_shape_check,
    DROP CONSTRAINT profile_versions_digest_sha256_check;
ALTER TABLE profiles DROP CONSTRAINT profiles_owner_id_key;
