-- name: GetEnvironmentCreatePin :one
SELECT
    e.owner_user_id,
    e.id AS environment_id,
    e.pinned_profile_version_id
FROM operations o
JOIN environments e ON e.id = o.environment_id
WHERE o.id = sqlc.arg(operation_id)
  AND o.type = 'environment.create';
