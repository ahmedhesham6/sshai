-- +goose Up
ALTER TABLE provider_resources
    DROP CONSTRAINT provider_resources_operation_id_environment_id_operation_t_fkey;
DROP TRIGGER provider_resources_identity_guard ON provider_resources;
DROP FUNCTION preserve_provider_resource_identity();

ALTER TABLE provider_resources DROP COLUMN operation_type;
ALTER TABLE provider_resources
    ADD COLUMN operation_type TEXT NOT NULL DEFAULT 'environment.create'
        CHECK (operation_type IN ('environment.create', 'runtime.start', 'runtime.replace')),
    ADD COLUMN runtime_id TEXT;
ALTER TABLE provider_resources DROP CONSTRAINT provider_resources_resource_type_check;
ALTER TABLE provider_resources
    ADD CONSTRAINT provider_resources_resource_type_check
        CHECK (resource_type IN ('data_volume', 'runtime', 'system_volume')),
    ADD CONSTRAINT provider_resources_runtime_shape_check CHECK (
        (resource_type = 'data_volume' AND runtime_id IS NULL)
        OR (resource_type IN ('runtime', 'system_volume') AND runtime_id IS NOT NULL)
    ),
    ADD CONSTRAINT provider_resources_runtime_fkey
        FOREIGN KEY (environment_id, runtime_id) REFERENCES runtimes (environment_id, id),
    ADD CONSTRAINT provider_resources_operation_fkey
        FOREIGN KEY (operation_id, environment_id, operation_type)
        REFERENCES operations (id, environment_id, type);

ALTER TABLE runtimes
    ADD COLUMN provider_resource_id TEXT,
    ADD COLUMN provider_resource_type TEXT GENERATED ALWAYS AS (
        CASE WHEN provider_resource_id IS NOT NULL THEN 'runtime'::TEXT END
    ) STORED,
    ADD CONSTRAINT runtimes_provider_resource_fkey
        FOREIGN KEY (environment_id, provider_resource_id, provider_resource_type)
        REFERENCES provider_resources (environment_id, id, resource_type);

-- +goose StatementBegin
CREATE FUNCTION synchronize_runtime_provider_identity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    inventory_provider_id TEXT;
BEGIN
    IF NEW.provider_resource_id IS NOT NULL AND NEW.status <> 'absent' THEN
        SELECT provider_id INTO STRICT inventory_provider_id
          FROM provider_resources
         WHERE environment_id = NEW.environment_id
           AND id = NEW.provider_resource_id
           AND resource_type = 'runtime';
        IF NEW.provider_instance_ref IS NULL THEN
            NEW.provider_instance_ref := inventory_provider_id;
        ELSIF NEW.provider_instance_ref IS DISTINCT FROM inventory_provider_id THEN
            RAISE EXCEPTION 'Runtime provider identity differs from Provider Resource inventory'
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runtimes_provider_identity_guard
    BEFORE INSERT OR UPDATE OF status, provider_resource_id, provider_instance_ref ON runtimes
    FOR EACH ROW EXECUTE FUNCTION synchronize_runtime_provider_identity();

-- +goose StatementBegin
CREATE FUNCTION preserve_provider_resource_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
       OR NEW.runtime_id IS DISTINCT FROM OLD.runtime_id
       OR NEW.operation_id IS DISTINCT FROM OLD.operation_id
       OR NEW.operation_type IS DISTINCT FROM OLD.operation_type
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

-- Runtime and system-volume inventory is lifecycle history, not Environment
-- durable-state inventory. Keep the aggregate invariant scoped to its one
-- non-compensatable Data Volume backend.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION validate_environment_state(target_environment_id TEXT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    target_lifecycle TEXT;
    component_count BIGINT;
    component_backend_count BIGINT;
    live_backend_count BIGINT;
    referenced_live_backend_count BIGINT;
BEGIN
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
     WHERE environment_id = target_environment_id
       AND resource_type = 'data_volume';
    SELECT count(DISTINCT resource.id) FILTER (WHERE resource.deleted_at IS NULL)
      INTO referenced_live_backend_count
      FROM state_components component
      JOIN provider_resources resource
        ON resource.environment_id = component.environment_id
       AND resource.id = component.backend_resource_id
       AND resource.resource_type = 'data_volume'
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

-- +goose Down
DROP TRIGGER runtimes_provider_identity_guard ON runtimes;
DROP FUNCTION synchronize_runtime_provider_identity();
ALTER TABLE runtimes DROP CONSTRAINT runtimes_provider_resource_fkey;
ALTER TABLE runtimes DROP COLUMN provider_resource_type;
ALTER TABLE runtimes DROP COLUMN provider_resource_id;

DROP TRIGGER provider_resources_identity_guard ON provider_resources;
DROP FUNCTION preserve_provider_resource_identity();
DELETE FROM provider_resources WHERE resource_type IN ('runtime', 'system_volume');
ALTER TABLE provider_resources DROP CONSTRAINT provider_resources_operation_fkey;
ALTER TABLE provider_resources DROP CONSTRAINT provider_resources_runtime_fkey;
ALTER TABLE provider_resources DROP CONSTRAINT provider_resources_runtime_shape_check;
ALTER TABLE provider_resources DROP CONSTRAINT provider_resources_resource_type_check;
ALTER TABLE provider_resources DROP COLUMN runtime_id;
ALTER TABLE provider_resources DROP COLUMN operation_type;
ALTER TABLE provider_resources
    ADD COLUMN operation_type TEXT GENERATED ALWAYS AS ('environment.create'::TEXT) STORED,
    ADD CONSTRAINT provider_resources_resource_type_check CHECK (resource_type = 'data_volume'),
    ADD CONSTRAINT provider_resources_operation_id_environment_id_operation_t_fkey
        FOREIGN KEY (operation_id, environment_id, operation_type)
        REFERENCES operations (id, environment_id, type);

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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION validate_environment_state(target_environment_id TEXT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    target_lifecycle TEXT;
    component_count BIGINT;
    component_backend_count BIGINT;
    live_backend_count BIGINT;
    referenced_live_backend_count BIGINT;
BEGIN
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
