-- name: GetOwnedOperation :one
SELECT id, environment_id, type, status, requested_by_user_id, idempotency_key,
       restate_invocation_id, input, created_at, completed_at
FROM operations
WHERE id = sqlc.arg(operation_id) AND requested_by_user_id = sqlc.arg(owner_user_id);

-- name: ListOperationSteps :many
SELECT step_key, status, summary
FROM operation_steps
WHERE operation_id = sqlc.arg(operation_id)
ORDER BY step_key;

-- name: ListOwnedEnvironmentOperations :many
SELECT o.id, o.environment_id, o.type, o.status, o.created_at
FROM operations o
JOIN environments e ON e.id = o.environment_id
WHERE e.id = sqlc.arg(environment_id) AND e.owner_user_id = sqlc.arg(owner_user_id)
ORDER BY o.created_at, o.id;
