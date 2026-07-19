-- +goose Up
CREATE INDEX provider_resources_deleted_at_retention_idx
    ON provider_resources (deleted_at)
    WHERE deleted_at IS NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION preserve_provider_resource_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.deleted_at IS NULL OR OLD.deleted_at >= CURRENT_TIMESTAMP - INTERVAL '30 days' THEN
            RAISE EXCEPTION 'Provider Resource lifecycle history is still retained'
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN OLD;
    END IF;
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

-- +goose Down
DROP INDEX provider_resources_deleted_at_retention_idx;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION preserve_provider_resource_identity() RETURNS trigger LANGUAGE plpgsql AS $$
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
