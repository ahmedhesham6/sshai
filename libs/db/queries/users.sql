-- name: EnsureUser :one
INSERT INTO users (
    id,
    workos_user_id,
    default_region,
    created_at,
    updated_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(workos_user_id),
    sqlc.arg(default_region),
    sqlc.arg(observed_at),
    sqlc.arg(observed_at)
)
ON CONFLICT (workos_user_id) DO UPDATE
SET updated_at = GREATEST(users.updated_at, EXCLUDED.updated_at)
RETURNING id, workos_user_id, default_region, created_at, updated_at;
