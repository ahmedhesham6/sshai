-- name: GetEnvironmentPin :one
SELECT id AS environment_id, capsule_lock_id, upgrade_policy
FROM environments
WHERE id = sqlc.arg(environment_id);

-- name: GetCapsuleLockForEnvironment :one
SELECT id, environment_id, profile_version_id, project_capsule_digest, digest,
       capsules, resolved_components, created_at
FROM capsule_locks
WHERE id = sqlc.arg(lock_id)
  AND environment_id = sqlc.arg(environment_id);

-- name: UpsertEnvironmentPin :one
UPDATE environments
SET capsule_lock_id = sqlc.narg(capsule_lock_id),
    upgrade_policy = sqlc.arg(upgrade_policy)
WHERE environments.id = sqlc.arg(environment_id)
  AND (
      sqlc.narg(capsule_lock_id)::text IS NULL
      OR EXISTS (
          SELECT 1
          FROM capsule_locks
          WHERE capsule_locks.id = sqlc.narg(capsule_lock_id)
            AND capsule_locks.environment_id = environments.id
      )
  )
RETURNING environments.id AS environment_id, capsule_lock_id, upgrade_policy;

-- name: ListEnvironmentMaterializations :many
SELECT environment_id, lock_id, id, lock_digest, capsule_digest, component_id, component_digest,
       adapter_id, adapter_version, target_agent_version, scope, component_type, trust_class,
       non_secret_overrides_digest, secret_version_identifiers, effective_cache_key,
       mode, root, target, selector, directory, file_paths,
       last_applied_digest, observed_digest, credential_requirement_digest, created_at, updated_at
FROM environment_materializations
WHERE environment_id = sqlc.arg(environment_id)
ORDER BY component_id;

-- name: UpsertEnvironmentMaterialization :execrows
INSERT INTO environment_materializations (
    environment_id, lock_id, id, lock_digest, capsule_digest, component_id, component_digest,
    adapter_id, adapter_version, target_agent_version, scope, component_type, trust_class,
    non_secret_overrides_digest, secret_version_identifiers, effective_cache_key,
    mode, root, target, selector, directory, file_paths,
    last_applied_digest, observed_digest, credential_requirement_digest, created_at, updated_at
)
SELECT
    sqlc.arg(environment_id), sqlc.arg(lock_id), sqlc.narg(id), sqlc.arg(lock_digest), sqlc.arg(capsule_digest),
    sqlc.arg(component_id), sqlc.arg(component_digest), sqlc.arg(adapter_id), sqlc.arg(adapter_version),
    sqlc.arg(target_agent_version), sqlc.arg(scope), sqlc.arg(component_type), sqlc.arg(trust_class),
    sqlc.narg(non_secret_overrides_digest), sqlc.arg(secret_version_identifiers), sqlc.arg(effective_cache_key),
    sqlc.narg(mode), sqlc.narg(root), sqlc.narg(target), sqlc.narg(selector), sqlc.arg(directory), sqlc.arg(file_paths),
    sqlc.narg(last_applied_digest), sqlc.narg(observed_digest), sqlc.narg(credential_requirement_digest),
    sqlc.arg(created_at), sqlc.arg(updated_at)
FROM capsule_locks
WHERE capsule_locks.id = sqlc.arg(lock_id)
  AND capsule_locks.environment_id = sqlc.arg(environment_id)
ON CONFLICT (environment_id, component_id) DO UPDATE
SET lock_id = EXCLUDED.lock_id,
    id = EXCLUDED.id,
    lock_digest = EXCLUDED.lock_digest,
    capsule_digest = EXCLUDED.capsule_digest,
    component_digest = EXCLUDED.component_digest,
    adapter_id = EXCLUDED.adapter_id,
    adapter_version = EXCLUDED.adapter_version,
    target_agent_version = EXCLUDED.target_agent_version,
    scope = EXCLUDED.scope,
    component_type = EXCLUDED.component_type,
    trust_class = EXCLUDED.trust_class,
    non_secret_overrides_digest = EXCLUDED.non_secret_overrides_digest,
    secret_version_identifiers = EXCLUDED.secret_version_identifiers,
    effective_cache_key = EXCLUDED.effective_cache_key,
    mode = EXCLUDED.mode,
    root = EXCLUDED.root,
    target = EXCLUDED.target,
    selector = EXCLUDED.selector,
    directory = EXCLUDED.directory,
    file_paths = EXCLUDED.file_paths,
    last_applied_digest = EXCLUDED.last_applied_digest,
    observed_digest = EXCLUDED.observed_digest,
    credential_requirement_digest = EXCLUDED.credential_requirement_digest,
    updated_at = EXCLUDED.updated_at;

-- name: DeleteEnvironmentMaterializations :execrows
DELETE FROM environment_materializations
WHERE environment_id = sqlc.arg(environment_id);
