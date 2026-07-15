-- +goose Up
CREATE TABLE upload_intents (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    kind TEXT NOT NULL CHECK (kind IN (
        'profile_artifact', 'git_bundle', 'tracked_patch', 'untracked_bundle', 'seed_manifest'
    )),
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    object_key TEXT NOT NULL UNIQUE CHECK (object_key <> ''),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at > created_at),
    UNIQUE (owner_user_id, id)
);

CREATE INDEX upload_intents_owned_digest_idx
    ON upload_intents (owner_user_id, kind, digest, created_at DESC, id DESC);

CREATE TABLE upload_intent_registrations (
    owner_user_id TEXT NOT NULL REFERENCES users (id),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    input JSONB NOT NULL,
    upload_intent_id TEXT NOT NULL,
    PRIMARY KEY (owner_user_id, idempotency_key),
    FOREIGN KEY (owner_user_id, upload_intent_id)
        REFERENCES upload_intents (owner_user_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION reject_upload_intent_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE check_violation USING MESSAGE = 'Upload Intents are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER upload_intents_immutable
BEFORE UPDATE OR DELETE ON upload_intents
FOR EACH ROW EXECUTE FUNCTION reject_upload_intent_mutation();

-- +goose Down
DROP TABLE upload_intent_registrations;
DROP TRIGGER upload_intents_immutable ON upload_intents;
DROP FUNCTION reject_upload_intent_mutation;
DROP TABLE upload_intents;
