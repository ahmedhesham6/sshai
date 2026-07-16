-- +goose Up
CREATE TABLE profile_version_capsule_refs (
    profile_version_id TEXT NOT NULL REFERENCES profile_versions (id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    ref TEXT NOT NULL CHECK (ref <> ''),
    freshness_policy TEXT NOT NULL CHECK (freshness_policy IN ('track', 'review', 'pin')),
    exclusions JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(exclusions) = 'array'),
    PRIMARY KEY (profile_version_id, ordinal)
);

-- +goose StatementBegin
CREATE FUNCTION reject_immutable_profile_version_capsule_refs() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE check_violation USING MESSAGE = 'Profile Version Capsule Refs are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER profile_version_capsule_refs_immutable
BEFORE UPDATE OR DELETE ON profile_version_capsule_refs
FOR EACH ROW EXECUTE FUNCTION reject_immutable_profile_version_capsule_refs();

-- +goose Down
DROP TRIGGER profile_version_capsule_refs_immutable ON profile_version_capsule_refs;
DROP FUNCTION reject_immutable_profile_version_capsule_refs;
DROP TABLE profile_version_capsule_refs;
