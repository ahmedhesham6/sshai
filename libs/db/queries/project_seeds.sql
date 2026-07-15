-- name: LockProjectSeedRegistration :one
SELECT pg_advisory_xact_lock(
    hashtextextended('project-seed' || chr(31) || sqlc.arg(owner_user_id)::text || chr(31) || sqlc.arg(idempotency_key)::text, 0)
);

-- name: LockProjectSeedDigest :one
SELECT pg_advisory_xact_lock(
    hashtextextended('project-seed-digest' || chr(31) || sqlc.arg(owner_user_id)::text || chr(31) || sqlc.arg(digest)::text, 0)
);

-- name: GetProjectSeedRegistration :one
SELECT
    ps.id,
    ps.owner_user_id,
    ps.repository_url,
    ps.base_revision,
    ps.digest,
    ps.git_bundle_digest,
    ps.tracked_patch_digest,
    ps.untracked_bundle_digest,
    ps.manifest_digest,
    ps.created_at,
    registration.input
FROM project_seed_registrations registration
JOIN project_seeds ps ON ps.id = registration.project_seed_id
WHERE registration.owner_user_id = sqlc.arg(owner_user_id)
  AND registration.idempotency_key = sqlc.arg(idempotency_key);

-- name: GetProjectSeedByDigest :one
SELECT
    id,
    owner_user_id,
    repository_url,
    base_revision,
    digest,
    git_bundle_digest,
    tracked_patch_digest,
    untracked_bundle_digest,
    manifest_digest,
    created_at
FROM project_seeds
WHERE owner_user_id = sqlc.arg(owner_user_id)
  AND digest = sqlc.arg(digest);

-- name: InsertProjectSeed :one
INSERT INTO project_seeds (
    id,
    owner_user_id,
    repository_url,
    base_revision,
    digest,
    git_bundle_digest,
    tracked_patch_digest,
    untracked_bundle_digest,
    manifest_digest,
    created_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(owner_user_id),
    sqlc.arg(repository_url),
    sqlc.arg(base_revision),
    sqlc.arg(digest),
    sqlc.narg(git_bundle_digest),
    sqlc.narg(tracked_patch_digest),
    sqlc.narg(untracked_bundle_digest),
    sqlc.arg(manifest_digest),
    sqlc.arg(created_at)
)
RETURNING
    id,
    owner_user_id,
    repository_url,
    base_revision,
    digest,
    git_bundle_digest,
    tracked_patch_digest,
    untracked_bundle_digest,
    manifest_digest,
    created_at;

-- name: InsertProjectSeedRegistration :exec
INSERT INTO project_seed_registrations (
    owner_user_id,
    idempotency_key,
    input,
    project_seed_id
) VALUES (
    sqlc.arg(owner_user_id),
    sqlc.arg(idempotency_key),
    sqlc.arg(input),
    sqlc.arg(project_seed_id)
);
