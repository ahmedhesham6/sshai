-- name: ListEnvironmentStateBackendsByOperation :many
SELECT id, environment_id, operation_id, provider, region, provider_id,
       metadata, created_at, deleted_at
FROM provider_resources
WHERE operation_id = sqlc.arg(operation_id)
  AND resource_type = 'data_volume'
ORDER BY created_at, id;

-- name: ListEnvironmentStateComponents :many
SELECT id, environment_id, kind, durability, mount_path, backend_resource_id,
       health, observed_digest, created_at, updated_at
FROM state_components
WHERE environment_id = sqlc.arg(environment_id)
ORDER BY CASE kind
    WHEN 'workspace' THEN 1
    WHEN 'home' THEN 2
    WHEN 'services' THEN 3
    WHEN 'cache' THEN 4
END;

-- name: InsertEnvironmentStateBackend :exec
INSERT INTO provider_resources (
    id, environment_id, operation_id, provider, region, resource_type,
    provider_id, metadata, created_at, deleted_at
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(operation_id),
    sqlc.arg(provider), sqlc.arg(region), 'data_volume', sqlc.arg(provider_id),
    sqlc.arg(metadata), sqlc.arg(created_at), sqlc.narg(deleted_at)
);

-- name: InsertEnvironmentStateComponent :exec
INSERT INTO state_components (
    id, environment_id, kind, durability, mount_path, backend_resource_id,
    backend_resource_type, health, observed_digest, created_at, updated_at
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(kind), sqlc.arg(durability),
    sqlc.arg(mount_path), sqlc.arg(backend_resource_id), 'data_volume',
    sqlc.arg(health), sqlc.narg(observed_digest), sqlc.arg(created_at), sqlc.arg(updated_at)
);
