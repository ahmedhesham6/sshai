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

-- name: InsertProfileVersion :exec
INSERT INTO profile_versions (id, profile_id, parent_version_id, version, digest, created_at)
VALUES (sqlc.arg(id), sqlc.arg(profile_id), sqlc.narg(parent_version_id), sqlc.arg(version), sqlc.arg(digest), sqlc.arg(created_at));

-- name: InsertProfileVersionCapsuleRef :exec
INSERT INTO profile_version_capsule_refs (profile_version_id, ordinal, ref, freshness_policy, exclusions)
VALUES (sqlc.arg(profile_version_id), sqlc.arg(ordinal), sqlc.arg(ref), sqlc.arg(freshness_policy), sqlc.arg(exclusions));

-- name: ListProfileVersionCapsuleRefs :many
SELECT ordinal, ref, freshness_policy, exclusions
FROM profile_version_capsule_refs
WHERE profile_version_id = sqlc.arg(profile_version_id)
ORDER BY ordinal;

-- name: GetProfileVersionForEnvironment :one
SELECT pv.id, pv.profile_id, pv.parent_version_id, pv.version, pv.digest, pv.created_at
FROM profile_versions pv
JOIN profiles p ON p.id = pv.profile_id
JOIN environments e ON e.owner_user_id = p.owner_user_id
WHERE e.id = sqlc.arg(environment_id)
  AND pv.id = sqlc.arg(profile_version_id)
FOR SHARE;

-- name: InsertProfilePublicationRegistration :exec
INSERT INTO profile_publication_registrations (owner_user_id, idempotency_key, input, profile_id, profile_version_id)
VALUES (sqlc.arg(owner_user_id), sqlc.arg(idempotency_key), sqlc.arg(input), sqlc.arg(profile_id), sqlc.arg(profile_version_id));

-- name: GetEnvironmentOwner :one
SELECT owner_user_id
FROM environments
WHERE id = sqlc.arg(environment_id)
FOR SHARE;

-- name: GetProfileResolveOperation :one
SELECT id, environment_id, type, status, requested_by_user_id, idempotency_key,
       restate_invocation_id, input, created_at, completed_at
FROM operations
WHERE id = sqlc.arg(operation_id)
  AND type = 'profile.resolve'
FOR UPDATE;

-- name: InsertProfileResolveOperation :exec
INSERT INTO operations (
    id, environment_id, type, status, requested_by_user_id, idempotency_key,
    restate_invocation_id, input, created_at, completed_at
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), 'profile.resolve', sqlc.arg(status),
    sqlc.arg(requested_by_user_id), sqlc.arg(idempotency_key),
    sqlc.arg(restate_invocation_id), sqlc.arg(input), sqlc.arg(created_at), sqlc.arg(completed_at)
);

-- name: InsertProfileResolveStep :exec
INSERT INTO operation_steps (
    id, operation_id, step_key, status, attempt, summary, started_at
) VALUES (
    sqlc.arg(id), sqlc.arg(operation_id), 'resolve', 'running', 1, 'Resolve Profile Version into Capsule Lock', sqlc.arg(started_at)
);

-- name: CompleteProfileResolveOperation :execrows
UPDATE operations
SET status = 'succeeded', completed_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(operation_id)
  AND type = 'profile.resolve'
  AND status IN ('queued', 'running');

-- name: CompleteProfileResolveStep :execrows
UPDATE operation_steps
SET status = 'succeeded', completed_at = sqlc.arg(completed_at)
WHERE operation_id = sqlc.arg(operation_id)
  AND step_key = 'resolve'
  AND status = 'running';

-- name: GetCapsuleLockByTarget :one
SELECT id, environment_id, profile_version_id, project_capsule_digest, digest,
       capsules, resolved_components, created_at
FROM capsule_locks
WHERE environment_id = sqlc.arg(environment_id)
  AND profile_version_id = sqlc.arg(profile_version_id)
  AND project_capsule_digest = sqlc.arg(project_capsule_digest);

-- name: InsertCapsuleLock :exec
INSERT INTO capsule_locks (
    id, environment_id, profile_version_id, project_capsule_digest, digest,
    capsules, resolved_components, created_at
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(profile_version_id), sqlc.arg(project_capsule_digest), sqlc.arg(digest),
    sqlc.arg(capsules), sqlc.arg(resolved_components), sqlc.arg(created_at)
);
