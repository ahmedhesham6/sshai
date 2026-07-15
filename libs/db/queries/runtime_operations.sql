-- name: LockRuntimeOperationIdempotency :one
SELECT pg_advisory_xact_lock(
    hashtextextended('runtime-operation' || chr(31) || sqlc.arg(owner_user_id)::text || chr(31) || sqlc.arg(idempotency_key)::text, 0)
);

-- name: GetOperationByIdempotencyKey :one
SELECT id, environment_id, type, status, requested_by_user_id, idempotency_key,
       restate_invocation_id, input, created_at, completed_at
FROM operations
WHERE requested_by_user_id = sqlc.arg(owner_user_id)
  AND idempotency_key = sqlc.arg(idempotency_key);

-- name: GetOwnedRuntimeStateForUpdate :one
SELECT
    e.id AS environment_id,
    e.owner_user_id,
    e.name,
    e.slug,
    e.lifecycle,
    e.health,
    e.region AS environment_region,
    e.availability_zone AS environment_availability_zone,
    e.runtime_preset AS environment_runtime_preset,
    e.pinned_profile_version_id,
    e.current_runtime_id,
    p.id AS auto_stop_policy_id,
    e.created_at AS environment_created_at,
    e.updated_at AS environment_updated_at,
    e.deleted_at,
    e.version AS environment_version,
    r.id AS runtime_id,
    r.environment_id AS runtime_environment_id,
    r.sequence,
    r.status AS runtime_status,
    r.runtime_preset AS runtime_runtime_preset,
    r.region AS runtime_region,
    r.availability_zone AS runtime_availability_zone,
    r.image_version,
    r.provider_instance_ref,
    r.private_address,
    r.boot_id,
    r.started_at,
    r.stopped_at,
    r.retired_at,
    r.created_at AS runtime_created_at,
    r.updated_at AS runtime_updated_at,
    r.version AS runtime_version
FROM environments e
JOIN auto_stop_policies p ON p.environment_id = e.id
JOIN runtimes r
  ON r.environment_id = e.id
 AND r.id = COALESCE(sqlc.narg(runtime_id), e.current_runtime_id)
WHERE e.id = sqlc.arg(environment_id)
  AND e.owner_user_id = sqlc.arg(owner_user_id)
FOR UPDATE OF e, r;

-- name: GetRuntimeOperationTarget :one
SELECT runtime_id
FROM runtime_operation_targets
WHERE operation_id = sqlc.arg(operation_id);

-- name: InsertRuntimeOperation :exec
INSERT INTO operations (
    id, environment_id, type, status, requested_by_user_id, idempotency_key, input, created_at
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(type), sqlc.arg(status),
    sqlc.arg(requested_by_user_id), sqlc.arg(idempotency_key), sqlc.arg(input), sqlc.arg(created_at)
);

-- name: InsertRuntimeOperationOutbox :exec
INSERT INTO workflow_outbox (operation_id, kind, created_at)
VALUES (sqlc.arg(operation_id), sqlc.arg(kind), sqlc.arg(created_at));

-- name: InsertRuntimeOperationTarget :exec
INSERT INTO runtime_operation_targets (operation_id, environment_id, runtime_id, operation_type)
VALUES (sqlc.arg(operation_id), sqlc.arg(environment_id), sqlc.arg(runtime_id), sqlc.arg(operation_type));

-- name: GetPendingRuntimeOperation :one
SELECT outbox.operation_id, target.operation_type, target.environment_id, target.runtime_id
FROM workflow_outbox outbox
JOIN runtime_operation_targets target
  ON target.operation_id = outbox.operation_id
 AND target.operation_type = outbox.kind
WHERE outbox.operation_id = sqlc.arg(operation_id)
  AND outbox.started_at IS NULL
  AND outbox.kind IN ('runtime.start', 'runtime.stop', 'runtime.replace');

-- name: ListPendingRuntimeOperations :many
SELECT outbox.operation_id, target.operation_type, target.environment_id, target.runtime_id
FROM workflow_outbox outbox
JOIN runtime_operation_targets target
  ON target.operation_id = outbox.operation_id
 AND target.operation_type = outbox.kind
WHERE outbox.started_at IS NULL
  AND outbox.kind IN ('runtime.start', 'runtime.stop', 'runtime.replace')
ORDER BY outbox.created_at, outbox.operation_id
LIMIT sqlc.arg(limit_count);
