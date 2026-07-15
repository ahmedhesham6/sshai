-- +goose Up
-- Provider ownership cannot be reconstructed safely after a legacy creation
-- workflow may have started, so only provably unstarted creation may proceed.
LOCK TABLE environments, operations, workflow_outbox IN SHARE ROW EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM environments environment
        WHERE environment.lifecycle IN ('active', 'deleting')
           OR (
                environment.lifecycle = 'creating'
                AND (
                    (SELECT count(*) FROM operations operation
                     WHERE operation.environment_id = environment.id
                       AND operation.type = 'environment.create') <> 1
                    OR NOT EXISTS (
                        SELECT 1
                        FROM operations operation
                        JOIN workflow_outbox outbox ON outbox.operation_id = operation.id
                        WHERE operation.environment_id = environment.id
                          AND operation.type = 'environment.create'
                          AND operation.status = 'queued'
                          AND operation.restate_invocation_id IS NULL
                          AND operation.completed_at IS NULL
                          AND outbox.started_at IS NULL
                          AND outbox.restate_invocation_id IS NULL
                    )
                )
           )
    ) THEN
        RAISE EXCEPTION 'migration 00013 requires ambiguous Environment creation to be reconciled first'
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE environments ADD CONSTRAINT environments_provider_region_key UNIQUE (id, region);

CREATE TABLE provider_resources (
    id TEXT PRIMARY KEY CHECK (id <> '' AND id = sshai_trim_space(id)),
    environment_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    operation_type TEXT GENERATED ALWAYS AS ('environment.create'::TEXT) STORED,
    provider TEXT NOT NULL CHECK (provider <> '' AND provider = sshai_trim_space(provider)),
    region TEXT NOT NULL CHECK (region <> '' AND region = sshai_trim_space(region)),
    resource_type TEXT NOT NULL CHECK (resource_type = 'data_volume'),
    provider_id TEXT NOT NULL CHECK (provider_id <> '' AND provider_id = sshai_trim_space(provider_id)),
    metadata JSONB NOT NULL CHECK (jsonb_typeof(metadata) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    UNIQUE (environment_id, id, resource_type),
    CONSTRAINT provider_resources_provider_identity_key
        UNIQUE (provider, region, resource_type, provider_id),
    FOREIGN KEY (environment_id, region) REFERENCES environments (id, region),
    FOREIGN KEY (operation_id, environment_id, operation_type)
        REFERENCES operations (id, environment_id, type),
    CHECK (deleted_at IS NULL OR deleted_at >= created_at)
);

-- +goose StatementBegin
CREATE FUNCTION preserve_provider_resource_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
       OR NEW.operation_id IS DISTINCT FROM OLD.operation_id
       OR NEW.provider IS DISTINCT FROM OLD.provider
       OR NEW.region IS DISTINCT FROM OLD.region
       OR NEW.resource_type IS DISTINCT FROM OLD.resource_type
       OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS DISTINCT FROM OLD.deleted_at) THEN
        RAISE EXCEPTION 'Provider Resource identity and ownership are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER provider_resources_identity_guard
    BEFORE UPDATE OR DELETE ON provider_resources
    FOR EACH ROW EXECUTE FUNCTION preserve_provider_resource_identity();

CREATE TABLE state_components (
    id TEXT PRIMARY KEY CHECK (id <> '' AND id = sshai_trim_space(id)),
    environment_id TEXT NOT NULL REFERENCES environments (id),
    kind TEXT NOT NULL CHECK (kind IN ('workspace', 'home', 'services', 'cache')),
    durability TEXT NOT NULL CHECK (durability IN ('durable', 'disposable')),
    mount_path TEXT NOT NULL CHECK (mount_path <> ''),
    backend_resource_id TEXT NOT NULL,
    backend_resource_type TEXT NOT NULL CHECK (backend_resource_type = 'data_volume'),
    health TEXT NOT NULL CHECK (health IN ('healthy', 'degraded', 'blocked', 'unknown')),
    observed_digest TEXT CHECK (observed_digest IS NULL OR observed_digest ~ '^sha256:[a-f0-9]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (environment_id, kind),
    FOREIGN KEY (environment_id, backend_resource_id, backend_resource_type)
        REFERENCES provider_resources (environment_id, id, resource_type),
    CHECK (updated_at >= created_at),
    CHECK (
        (kind = 'workspace' AND durability = 'durable' AND mount_path = '/workspace')
        OR (kind = 'home' AND durability = 'durable' AND mount_path = '/home/dev')
        OR (kind = 'services' AND durability = 'durable' AND mount_path = '/var/lib/docker')
        OR (kind = 'cache' AND durability = 'disposable' AND mount_path = '/var/cache/devm')
    )
);

-- +goose StatementBegin
CREATE FUNCTION preserve_state_component_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
       OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.durability IS DISTINCT FROM OLD.durability
       OR NEW.mount_path IS DISTINCT FROM OLD.mount_path
       OR NEW.backend_resource_id IS DISTINCT FROM OLD.backend_resource_id
       OR NEW.backend_resource_type IS DISTINCT FROM OLD.backend_resource_type
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'State Component identity and policy are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER state_components_identity_guard
    BEFORE UPDATE ON state_components
    FOR EACH ROW EXECUTE FUNCTION preserve_state_component_identity();

-- +goose StatementBegin
CREATE FUNCTION validate_environment_state(target_environment_id TEXT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    target_lifecycle TEXT;
    component_count BIGINT;
    component_backend_count BIGINT;
    live_backend_count BIGINT;
    referenced_live_backend_count BIGINT;
BEGIN
    -- Lock the aggregate owner before reading related rows. The next validator
    -- observes any transaction that committed while waiting for this lock.
    PERFORM 1 FROM environments WHERE id = target_environment_id FOR UPDATE;
    SELECT lifecycle INTO target_lifecycle FROM environments WHERE id = target_environment_id;
    IF target_lifecycle IS NULL THEN
        RETURN;
    END IF;

    SELECT count(*), count(DISTINCT backend_resource_id)
      INTO component_count, component_backend_count
      FROM state_components
     WHERE environment_id = target_environment_id;
    SELECT count(*) FILTER (WHERE deleted_at IS NULL)
      INTO live_backend_count
      FROM provider_resources
     WHERE environment_id = target_environment_id;
    SELECT count(DISTINCT resource.id) FILTER (WHERE resource.deleted_at IS NULL)
      INTO referenced_live_backend_count
      FROM state_components component
      JOIN provider_resources resource
        ON resource.environment_id = component.environment_id
       AND resource.id = component.backend_resource_id
     WHERE component.environment_id = target_environment_id;

    IF target_lifecycle = 'deleted' THEN
        IF component_count = 0 AND live_backend_count = 0 THEN
            RETURN;
        END IF;
    ELSIF target_lifecycle = 'creating' AND component_count = 0 AND live_backend_count = 0 THEN
        RETURN;
    ELSIF component_count = 4
       AND component_backend_count = 1
       AND live_backend_count = 1
       AND referenced_live_backend_count = 1 THEN
        RETURN;
    END IF;

    RAISE EXCEPTION 'Environment State inventory is incomplete or has no single live backend'
        USING ERRCODE = 'check_violation';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION validate_provider_resource_environment_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM validate_environment_state(COALESCE(NEW.environment_id, OLD.environment_id));
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION validate_state_component_environment_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM validate_environment_state(COALESCE(NEW.environment_id, OLD.environment_id));
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION validate_environment_lifecycle_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lifecycle IS DISTINCT FROM OLD.lifecycle THEN
        PERFORM validate_environment_state(NEW.id);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER provider_resources_environment_state_guard
    AFTER INSERT OR UPDATE OR DELETE ON provider_resources
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION validate_provider_resource_environment_state();

CREATE CONSTRAINT TRIGGER state_components_environment_state_guard
    AFTER INSERT OR UPDATE OR DELETE ON state_components
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION validate_state_component_environment_state();

CREATE CONSTRAINT TRIGGER environments_environment_state_guard
    AFTER UPDATE ON environments
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION validate_environment_lifecycle_state();

-- +goose Down
DROP TRIGGER environments_environment_state_guard ON environments;
DROP TRIGGER state_components_environment_state_guard ON state_components;
DROP TRIGGER provider_resources_environment_state_guard ON provider_resources;
DROP FUNCTION validate_environment_lifecycle_state();
DROP FUNCTION validate_state_component_environment_state();
DROP FUNCTION validate_provider_resource_environment_state();
DROP FUNCTION validate_environment_state(TEXT);
DROP TRIGGER state_components_identity_guard ON state_components;
DROP FUNCTION preserve_state_component_identity();
DROP TABLE state_components;
DROP TRIGGER provider_resources_identity_guard ON provider_resources;
DROP FUNCTION preserve_provider_resource_identity();
DROP TABLE provider_resources;
ALTER TABLE environments DROP CONSTRAINT environments_provider_region_key;
