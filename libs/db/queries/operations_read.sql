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
-- Keyset pagination: rows are ordered by (o.created_at, o.id), the same
-- tuple the WHERE predicate compares against the caller's decoded cursor.
-- Passing has_cursor = false selects the first page; row_limit is the
-- caller's effective page size plus one, letting the store detect a next
-- page without a second round trip.
SELECT o.id, o.environment_id, o.type, o.status, o.created_at
FROM operations o
JOIN environments e ON e.id = o.environment_id
WHERE e.id = sqlc.arg(environment_id) AND e.owner_user_id = sqlc.arg(owner_user_id)
  AND (
    NOT sqlc.arg(has_cursor)::bool
    OR (o.created_at, o.id) > (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::text)
  )
ORDER BY o.created_at, o.id
LIMIT sqlc.arg(row_limit)::int;
