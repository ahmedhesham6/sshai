-- name: GetInitialRuntimeForUpdate :one
SELECT id, environment_id, sequence, status, runtime_preset, region, availability_zone,
       image_version, provider_instance_ref, private_address, boot_id, started_at,
       stopped_at, retired_at, created_at, updated_at, version
FROM runtimes
WHERE id = sqlc.arg(runtime_id)
  AND environment_id = sqlc.arg(environment_id)
FOR UPDATE;

-- name: InsertInitialRuntime :exec
INSERT INTO runtimes (
    id, environment_id, sequence, status, runtime_preset, region, availability_zone,
    image_version, provider_instance_ref, private_address, boot_id, started_at,
    stopped_at, retired_at, created_at, updated_at, version
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(sequence), sqlc.arg(status),
    sqlc.arg(runtime_preset), sqlc.arg(region), sqlc.arg(availability_zone),
    sqlc.arg(image_version), sqlc.arg(provider_instance_ref), sqlc.arg(private_address),
    sqlc.arg(boot_id), sqlc.arg(started_at), sqlc.arg(stopped_at), sqlc.arg(retired_at),
    sqlc.arg(created_at), sqlc.arg(updated_at), sqlc.arg(version)
);

-- name: AttachInitialRuntime :execrows
UPDATE environments
SET current_runtime_id = sqlc.arg(runtime_id),
    updated_at = sqlc.arg(updated_at),
    version = sqlc.arg(next_version)
WHERE id = sqlc.arg(environment_id)
  AND current_runtime_id IS NULL
  AND version = sqlc.arg(current_version);
