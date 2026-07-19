-- name: InsertActivitySnapshot :execrows
INSERT INTO activity_snapshots (
    runtime_id, sequence, environment_id, observed_at,
    ssh_connections, ide_connections, codex_processes, claude_processes,
    protected_processes, selected_containers, unknown_user_processes
)
SELECT
    sqlc.arg(runtime_id), sqlc.arg(sequence), sqlc.arg(environment_id), sqlc.arg(observed_at),
    sqlc.arg(ssh_connections), sqlc.arg(ide_connections), sqlc.arg(codex_processes), sqlc.arg(claude_processes),
    sqlc.arg(protected_processes), sqlc.arg(selected_containers), sqlc.arg(unknown_user_processes)
FROM environments environment
JOIN runtimes runtime
  ON runtime.environment_id = environment.id
 AND runtime.id = environment.current_runtime_id
WHERE environment.id = sqlc.arg(environment_id)
  AND runtime.id = sqlc.arg(runtime_id)
ON CONFLICT (runtime_id, sequence) DO NOTHING;

-- name: GetAutoStopPolicyState :one
SELECT policy.id, policy.environment_id, policy.mode, policy.grace_period_seconds,
       policy.generation, environment.current_runtime_id
FROM auto_stop_policies policy
JOIN environments environment ON environment.id = policy.environment_id
WHERE policy.environment_id = sqlc.arg(environment_id);

-- name: GetLatestActivitySnapshot :one
SELECT runtime_id, sequence, environment_id, observed_at,
       ssh_connections, ide_connections, codex_processes, claude_processes,
       protected_processes, selected_containers, unknown_user_processes
FROM activity_snapshots
WHERE runtime_id = sqlc.arg(runtime_id)
ORDER BY sequence DESC
LIMIT 1;

-- name: GetActivitySnapshot :one
SELECT runtime_id, sequence, environment_id, observed_at,
       ssh_connections, ide_connections, codex_processes, claude_processes,
       protected_processes, selected_containers, unknown_user_processes
FROM activity_snapshots
WHERE runtime_id = sqlc.arg(runtime_id)
  AND environment_id = sqlc.arg(environment_id)
  AND sequence = sqlc.arg(sequence);

-- name: GetPendingAutoStopPolicyRefresh :one
SELECT environment_id, generation
FROM auto_stop_policies
WHERE environment_id = sqlc.arg(environment_id)
  AND refresh_acknowledged_generation < generation;

-- name: ListPendingAutoStopPolicyRefreshes :many
SELECT environment_id, generation
FROM auto_stop_policies
WHERE refresh_acknowledged_generation < generation
ORDER BY environment_id
LIMIT sqlc.arg(limit_count);

-- name: AcknowledgeAutoStopPolicyRefresh :execrows
UPDATE auto_stop_policies
SET refresh_acknowledged_generation = GREATEST(refresh_acknowledged_generation, sqlc.arg(generation))
WHERE environment_id = sqlc.arg(environment_id)
  AND generation >= sqlc.arg(generation);

-- name: ListActiveAutoStopOperationTypes :many
SELECT type
FROM operations
WHERE environment_id = sqlc.arg(environment_id)
  AND status IN ('queued', 'running')
  AND type IN ('environment.create', 'profile.resolve', 'runtime.start', 'runtime.replace')
ORDER BY type;

-- name: GetRuntimeStopDispatchOwner :one
SELECT owner_user_id, current_runtime_id
FROM environments
WHERE id = sqlc.arg(environment_id);
