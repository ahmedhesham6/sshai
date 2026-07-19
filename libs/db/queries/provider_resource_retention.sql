-- name: PruneProviderResources :execrows
WITH candidates AS MATERIALIZED (
    SELECT resource.id
    FROM provider_resources resource
    WHERE resource.deleted_at < sqlc.arg(retain_after)
      AND NOT EXISTS (
          SELECT 1
          FROM environments environment
          WHERE environment.id = resource.environment_id
            AND environment.current_runtime_id = resource.runtime_id
      )
      AND NOT EXISTS (
          SELECT 1
          FROM state_components component
          WHERE component.environment_id = resource.environment_id
            AND component.backend_resource_id = resource.id
      )
), cleared_runtime_references AS (
    UPDATE runtimes
    SET provider_resource_id = NULL
    WHERE provider_resource_id IN (SELECT id FROM candidates)
    RETURNING id
)
DELETE FROM provider_resources resource
USING candidates
WHERE resource.id = candidates.id;
