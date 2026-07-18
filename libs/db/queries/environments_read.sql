-- name: GetOwnedEnvironmentDetail :one
SELECT
    e.id, e.owner_user_id, e.name, e.slug, e.lifecycle, e.health,
    e.region AS environment_region, e.availability_zone AS environment_availability_zone,
    e.runtime_preset AS environment_runtime_preset, e.pinned_profile_version_id,
    e.capsule_lock_id, e.upgrade_policy, e.current_runtime_id,
    e.created_at AS environment_created_at, e.updated_at AS environment_updated_at,
    e.deleted_at, e.version AS environment_version,
    p.id AS auto_stop_policy_id, p.mode AS auto_stop_mode, p.grace_period_seconds,
    active_op.id AS active_operation_id
FROM environments e
JOIN auto_stop_policies p ON p.environment_id = e.id
LEFT JOIN operations active_op
    ON active_op.environment_id = e.id AND active_op.status IN ('queued', 'running')
WHERE e.id = sqlc.arg(environment_id) AND e.owner_user_id = sqlc.arg(owner_user_id);

-- name: ListOwnedEnvironmentDetails :many
SELECT
    e.id, e.owner_user_id, e.name, e.slug, e.lifecycle, e.health,
    e.region AS environment_region, e.availability_zone AS environment_availability_zone,
    e.runtime_preset AS environment_runtime_preset, e.pinned_profile_version_id,
    e.capsule_lock_id, e.upgrade_policy, e.current_runtime_id,
    e.created_at AS environment_created_at, e.updated_at AS environment_updated_at,
    e.deleted_at, e.version AS environment_version,
    p.id AS auto_stop_policy_id, p.mode AS auto_stop_mode, p.grace_period_seconds,
    active_op.id AS active_operation_id
FROM environments e
JOIN auto_stop_policies p ON p.environment_id = e.id
LEFT JOIN operations active_op
    ON active_op.environment_id = e.id AND active_op.status IN ('queued', 'running')
WHERE e.owner_user_id = sqlc.arg(owner_user_id)
ORDER BY e.created_at, e.id;

-- name: GetRuntimeByID :one
SELECT id, environment_id, sequence, status, runtime_preset, region, availability_zone,
       image_version, provider_instance_ref, private_address, boot_id,
       started_at, stopped_at, retired_at, created_at, updated_at, version
FROM runtimes
WHERE id = sqlc.arg(runtime_id);
