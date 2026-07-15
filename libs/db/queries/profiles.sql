-- name: LockProfileCreateRegistration :one
SELECT pg_advisory_xact_lock(
    hashtextextended('profile-create' || chr(31) || sqlc.arg(owner_user_id)::text || chr(31) || sqlc.arg(idempotency_key)::text, 0)
);

-- name: GetProfileCreateRegistration :one
SELECT p.id, p.owner_user_id, p.name, p.slug, p.created_at, p.archived_at, registration.input
FROM profile_create_registrations registration
JOIN profiles p ON p.id = registration.profile_id
WHERE registration.owner_user_id = sqlc.arg(owner_user_id)
  AND registration.idempotency_key = sqlc.arg(idempotency_key);

-- name: InsertProfile :exec
INSERT INTO profiles (id, owner_user_id, name, slug, created_at, archived_at)
VALUES (sqlc.arg(id), sqlc.arg(owner_user_id), sqlc.arg(name), sqlc.arg(slug), sqlc.arg(created_at), sqlc.narg(archived_at));

-- name: InsertProfileCreateRegistration :exec
INSERT INTO profile_create_registrations (owner_user_id, idempotency_key, input, profile_id)
VALUES (sqlc.arg(owner_user_id), sqlc.arg(idempotency_key), sqlc.arg(input), sqlc.arg(profile_id));

-- name: LockProfilePublicationRegistration :one
SELECT pg_advisory_xact_lock(
    hashtextextended('profile-publication' || chr(31) || sqlc.arg(owner_user_id)::text || chr(31) || sqlc.arg(idempotency_key)::text, 0)
);

-- name: GetProfilePublicationRegistration :one
SELECT registration.input, registration.profile_id, registration.profile_version_id
FROM profile_publication_registrations registration
WHERE registration.owner_user_id = sqlc.arg(owner_user_id)
  AND registration.idempotency_key = sqlc.arg(idempotency_key);

-- name: GetOwnedProfileForUpdate :one
SELECT id, owner_user_id, name, slug, created_at, archived_at
FROM profiles
WHERE id = sqlc.arg(profile_id) AND owner_user_id = sqlc.arg(owner_user_id)
FOR UPDATE;

-- name: GetProfileHead :one
SELECT id, profile_id, parent_version_id, version, digest, created_at
FROM profile_versions
WHERE profile_id = sqlc.arg(profile_id)
ORDER BY version DESC
LIMIT 1
FOR UPDATE;

-- name: GetProfileVersion :one
SELECT id, profile_id, parent_version_id, version, digest, created_at
FROM profile_versions
WHERE id = sqlc.arg(profile_version_id) AND profile_id = sqlc.arg(profile_id);

-- name: ListProfileArtifacts :many
SELECT id, profile_version_id, kind, source_locator, source_digest, content_digest, size_bytes, mode, sensitivity, trust, contains_executable
FROM profile_artifacts
WHERE profile_version_id = sqlc.arg(profile_version_id)
ORDER BY id;

-- name: InsertProfileVersion :exec
INSERT INTO profile_versions (id, profile_id, parent_version_id, version, digest, created_at)
VALUES (sqlc.arg(id), sqlc.arg(profile_id), sqlc.narg(parent_version_id), sqlc.arg(version), sqlc.arg(digest), sqlc.arg(created_at));

-- name: InsertProfileArtifact :exec
INSERT INTO profile_artifacts (
    id, profile_version_id, kind, source_locator, source_digest, content_digest, size_bytes, mode, sensitivity, trust, contains_executable
) VALUES (
    sqlc.arg(id), sqlc.arg(profile_version_id), sqlc.arg(kind), sqlc.arg(source_locator), sqlc.arg(source_digest),
    sqlc.arg(content_digest), sqlc.arg(size_bytes), sqlc.arg(mode), sqlc.arg(sensitivity), sqlc.arg(trust), sqlc.arg(contains_executable)
);

-- name: InsertProfilePublicationRegistration :exec
INSERT INTO profile_publication_registrations (owner_user_id, idempotency_key, input, profile_id, profile_version_id)
VALUES (sqlc.arg(owner_user_id), sqlc.arg(idempotency_key), sqlc.arg(input), sqlc.arg(profile_id), sqlc.arg(profile_version_id));
