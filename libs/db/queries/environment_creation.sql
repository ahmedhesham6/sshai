-- name: LockEnvironmentCreation :one
SELECT pg_advisory_xact_lock(
    hashtextextended(sqlc.arg(owner_user_id) || E'\x1f' || sqlc.arg(idempotency_key), 0)
);

-- name: GetEnvironmentCreationByKey :one
SELECT
    e.id AS environment_id,
    e.owner_user_id,
    e.name,
    e.slug,
    e.lifecycle,
    e.health,
    e.region,
    e.availability_zone,
    e.runtime_preset,
    e.pinned_profile_version_id,
    e.current_runtime_id,
    e.created_at AS environment_created_at,
    e.updated_at AS environment_updated_at,
    e.deleted_at,
    e.version,
    p.id AS policy_id,
    p.mode AS policy_mode,
    p.grace_period_seconds,
    o.id AS operation_id,
    o.type AS operation_type,
    o.status AS operation_status,
    o.idempotency_key,
    o.restate_invocation_id,
    o.input AS operation_input,
    o.created_at AS operation_created_at,
    o.completed_at AS operation_completed_at,
    ps.id AS project_seed_id
FROM operations o
JOIN environments e ON e.id = o.environment_id
JOIN auto_stop_policies p ON p.environment_id = e.id
JOIN project_seeds ps ON ps.environment_id = e.id
WHERE o.requested_by_user_id = sqlc.arg(owner_user_id)
  AND o.idempotency_key = sqlc.arg(idempotency_key);

-- name: ListEnvironmentSSHKeyIDs :many
SELECT ssh_key_id
FROM environment_ssh_keys
WHERE environment_id = sqlc.arg(environment_id)
ORDER BY ssh_key_id;

-- name: InsertEnvironment :execrows
INSERT INTO environments (
    id, owner_user_id, name, slug, lifecycle, health, region, availability_zone,
    runtime_preset, pinned_profile_version_id, current_runtime_id, created_at,
    updated_at, deleted_at, version
)
SELECT
    sqlc.arg(id), sqlc.arg(owner_user_id), sqlc.arg(name), sqlc.arg(slug),
    sqlc.arg(lifecycle), sqlc.arg(health), sqlc.arg(region), sqlc.arg(availability_zone),
    sqlc.arg(runtime_preset), sqlc.arg(pinned_profile_version_id), sqlc.arg(current_runtime_id),
    sqlc.arg(created_at), sqlc.arg(updated_at), sqlc.arg(deleted_at), sqlc.arg(version)
FROM profile_versions pv
JOIN profiles profile ON profile.id = pv.profile_id
WHERE pv.id = sqlc.arg(pinned_profile_version_id)
  AND profile.owner_user_id = sqlc.arg(owner_user_id);

-- name: InsertAutoStopPolicy :exec
INSERT INTO auto_stop_policies (
    id, environment_id, mode, grace_period_seconds
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(mode), sqlc.arg(grace_period_seconds)
);

-- name: AssignProjectSeed :execrows
UPDATE project_seeds
SET environment_id = sqlc.arg(environment_id)
WHERE id = sqlc.arg(project_seed_id)
  AND owner_user_id = sqlc.arg(owner_user_id)
  AND environment_id IS NULL;

-- name: AssignEnvironmentSSHKeys :execrows
INSERT INTO environment_ssh_keys (environment_id, owner_user_id, ssh_key_id)
SELECT sqlc.arg(environment_id), sqlc.arg(owner_user_id), key.id
FROM ssh_keys key
WHERE key.owner_user_id = sqlc.arg(owner_user_id)
  AND key.revoked_at IS NULL
  AND key.id = ANY(sqlc.arg(ssh_key_ids)::text[]);

-- name: InsertOperation :exec
INSERT INTO operations (
    id, environment_id, type, status, requested_by_user_id, idempotency_key,
    restate_invocation_id, input, created_at, completed_at
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(type), sqlc.arg(status),
    sqlc.arg(requested_by_user_id), sqlc.arg(idempotency_key),
    sqlc.arg(restate_invocation_id), sqlc.arg(input), sqlc.arg(created_at),
    sqlc.arg(completed_at)
);

-- name: InsertEnvironmentCreateOutbox :exec
INSERT INTO workflow_outbox (operation_id, kind, created_at)
VALUES (sqlc.arg(operation_id), 'environment.create', sqlc.arg(created_at));

-- name: GetPendingEnvironmentCreate :one
SELECT o.id AS operation_id, e.id AS environment_id, e.region, e.availability_zone
FROM workflow_outbox outbox
JOIN operations o ON o.id = outbox.operation_id
JOIN environments e ON e.id = o.environment_id
WHERE outbox.operation_id = sqlc.arg(operation_id)
  AND outbox.started_at IS NULL;

-- name: ListPendingEnvironmentCreates :many
SELECT o.id AS operation_id, e.id AS environment_id, e.region, e.availability_zone
FROM workflow_outbox outbox
JOIN operations o ON o.id = outbox.operation_id
JOIN environments e ON e.id = o.environment_id
WHERE outbox.started_at IS NULL
ORDER BY outbox.created_at, outbox.operation_id
LIMIT sqlc.arg(limit_count);

-- name: RecordOperationRestateInvocation :execrows
UPDATE operations
SET restate_invocation_id = sqlc.arg(restate_invocation_id)
WHERE id = sqlc.arg(operation_id)
  AND (restate_invocation_id IS NULL OR restate_invocation_id = sqlc.arg(restate_invocation_id));

-- name: MarkEnvironmentCreateOutboxStarted :execrows
UPDATE workflow_outbox
SET started_at = COALESCE(started_at, sqlc.arg(started_at)),
    restate_invocation_id = sqlc.arg(restate_invocation_id)
WHERE operation_id = sqlc.arg(operation_id)
  AND (started_at IS NULL OR restate_invocation_id = sqlc.arg(restate_invocation_id));

-- name: GetOperationCreationKeyForUpdate :one
SELECT requested_by_user_id, idempotency_key
FROM operations
WHERE id = sqlc.arg(operation_id)
  AND type = 'environment.create'
FOR UPDATE;

-- name: UpdateCreatedEnvironment :execrows
UPDATE environments
SET lifecycle = sqlc.arg(lifecycle),
    health = sqlc.arg(health),
    updated_at = sqlc.arg(updated_at),
    version = sqlc.arg(next_version)
WHERE id = sqlc.arg(environment_id)
  AND version = sqlc.arg(current_version);

-- name: CompleteCreatedEnvironmentOperation :execrows
UPDATE operations
SET status = sqlc.arg(status),
    completed_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(operation_id)
  AND status IN ('queued', 'running');
