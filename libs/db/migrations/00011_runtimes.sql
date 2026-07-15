-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION sshai_trim_space(value TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT btrim(
        value,
        chr(9) || chr(10) || chr(11) || chr(12) || chr(13) || chr(32)
        || chr(133) || chr(160) || chr(5760)
        || chr(8192) || chr(8193) || chr(8194) || chr(8195) || chr(8196)
        || chr(8197) || chr(8198) || chr(8199) || chr(8200) || chr(8201) || chr(8202)
        || chr(8232) || chr(8233) || chr(8239) || chr(8287) || chr(12288)
    )
$$;
-- +goose StatementEnd

ALTER TABLE environments ADD CONSTRAINT environments_runtime_placement_key
    UNIQUE (id, region, availability_zone, runtime_preset);
ALTER TABLE environments ADD CONSTRAINT environments_runtime_placement_canonical CHECK (
    region <> '' AND region = sshai_trim_space(region)
    AND availability_zone <> '' AND availability_zone = sshai_trim_space(availability_zone)
    AND runtime_preset <> '' AND runtime_preset = sshai_trim_space(runtime_preset)
);

CREATE TABLE runtimes (
    id TEXT PRIMARY KEY CHECK (id <> '' AND id = sshai_trim_space(id)),
    environment_id TEXT NOT NULL REFERENCES environments (id),
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    status TEXT NOT NULL CHECK (status IN (
        'absent', 'provisioning', 'starting', 'ready', 'stopping', 'stopped', 'replacing', 'error'
    )),
    runtime_preset TEXT NOT NULL CHECK (runtime_preset <> '' AND runtime_preset = sshai_trim_space(runtime_preset)),
    region TEXT NOT NULL CHECK (region <> '' AND region = sshai_trim_space(region)),
    availability_zone TEXT NOT NULL CHECK (availability_zone <> '' AND availability_zone = sshai_trim_space(availability_zone)),
    image_version TEXT NOT NULL CHECK (image_version <> '' AND image_version = sshai_trim_space(image_version)),
    provider_instance_ref TEXT CHECK (provider_instance_ref IS NULL OR (provider_instance_ref <> '' AND provider_instance_ref = sshai_trim_space(provider_instance_ref))),
    private_address TEXT CHECK (
        private_address IS NULL OR (
            family(private_address::inet) = 4
            AND host(private_address::inet) = private_address
            AND (
                private_address::inet <<= '10.0.0.0/8'::inet
                OR private_address::inet <<= '172.16.0.0/12'::inet
                OR private_address::inet <<= '192.168.0.0/16'::inet
            )
        )
    ),
    boot_id TEXT CHECK (boot_id IS NULL OR (boot_id <> '' AND boot_id = sshai_trim_space(boot_id))),
    started_at TIMESTAMPTZ,
    stopped_at TIMESTAMPTZ,
    retired_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    version BIGINT NOT NULL CHECK (version > 0),
    current_environment_id TEXT GENERATED ALWAYS AS (
        CASE WHEN retired_at IS NULL THEN environment_id END
    ) STORED,
    UNIQUE (environment_id, id),
    UNIQUE (environment_id, sequence),
    UNIQUE (current_environment_id, id),
    UNIQUE (current_environment_id),
    FOREIGN KEY (environment_id, region, availability_zone, runtime_preset)
        REFERENCES environments (id, region, availability_zone, runtime_preset),
    CHECK (updated_at >= created_at),
    CHECK (started_at IS NULL OR started_at BETWEEN created_at AND updated_at),
    CHECK (stopped_at IS NULL OR stopped_at BETWEEN created_at AND updated_at),
    CHECK (retired_at IS NULL OR retired_at BETWEEN created_at AND updated_at),
    CHECK (
        (status = 'absent' AND (
            (retired_at IS NULL AND provider_instance_ref IS NULL AND private_address IS NULL AND boot_id IS NULL AND started_at IS NULL AND stopped_at IS NULL)
            OR (retired_at IS NOT NULL AND provider_instance_ref IS NOT NULL AND private_address IS NULL AND boot_id IS NULL)
        ))
        OR (status = 'provisioning' AND provider_instance_ref IS NOT NULL AND started_at IS NULL AND stopped_at IS NULL AND retired_at IS NULL AND private_address IS NULL AND boot_id IS NULL)
        OR (status IN ('starting', 'stopping') AND provider_instance_ref IS NOT NULL AND started_at IS NOT NULL AND stopped_at IS NULL AND retired_at IS NULL AND private_address IS NULL AND boot_id IS NULL)
        OR (status = 'ready' AND provider_instance_ref IS NOT NULL AND started_at IS NOT NULL AND stopped_at IS NULL AND retired_at IS NULL AND private_address IS NOT NULL AND boot_id IS NOT NULL)
        OR (status = 'stopped' AND provider_instance_ref IS NOT NULL AND started_at IS NOT NULL AND stopped_at IS NOT NULL AND retired_at IS NULL AND private_address IS NULL AND boot_id IS NULL)
        OR (status IN ('replacing', 'error') AND provider_instance_ref IS NOT NULL AND retired_at IS NULL AND private_address IS NULL AND boot_id IS NULL)
    )
);

-- +goose StatementBegin
CREATE FUNCTION enforce_runtime_sequence() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    expected_sequence BIGINT;
BEGIN
    SELECT COALESCE(MAX(sequence), 0) + 1
      INTO expected_sequence
      FROM runtimes
     WHERE environment_id = NEW.environment_id;
    IF NEW.sequence <> expected_sequence THEN
        RAISE EXCEPTION 'Runtime sequence must be %', expected_sequence
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runtimes_sequence_guard
    BEFORE INSERT ON runtimes
    FOR EACH ROW EXECUTE FUNCTION enforce_runtime_sequence();

-- +goose StatementBegin
CREATE FUNCTION preserve_runtime_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
       OR NEW.sequence IS DISTINCT FROM OLD.sequence
       OR NEW.runtime_preset IS DISTINCT FROM OLD.runtime_preset
       OR NEW.region IS DISTINCT FROM OLD.region
       OR NEW.availability_zone IS DISTINCT FROM OLD.availability_zone
       OR NEW.image_version IS DISTINCT FROM OLD.image_version
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'Runtime identity and ownership are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runtimes_identity_guard
    BEFORE UPDATE ON runtimes
    FOR EACH ROW EXECUTE FUNCTION preserve_runtime_identity();

ALTER TABLE environments
    ADD CONSTRAINT environments_current_runtime_fkey
    FOREIGN KEY (id, current_runtime_id)
    REFERENCES runtimes (current_environment_id, id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE operations ADD CONSTRAINT operations_id_type_key UNIQUE (id, type);
ALTER TABLE operations ADD CONSTRAINT operations_id_environment_type_key UNIQUE (id, environment_id, type);

CREATE TABLE runtime_operation_targets (
    operation_id TEXT PRIMARY KEY REFERENCES operations (id) ON DELETE CASCADE,
    environment_id TEXT NOT NULL,
    runtime_id TEXT NOT NULL,
    operation_type TEXT NOT NULL CHECK (operation_type IN ('runtime.start', 'runtime.stop', 'runtime.replace')),
    FOREIGN KEY (environment_id, runtime_id) REFERENCES runtimes (environment_id, id),
    FOREIGN KEY (operation_id, environment_id, operation_type)
        REFERENCES operations (id, environment_id, type)
);

ALTER TABLE workflow_outbox DROP CONSTRAINT workflow_outbox_kind_check;
ALTER TABLE workflow_outbox ADD CONSTRAINT workflow_outbox_kind_check CHECK (kind IN (
    'environment.create', 'runtime.start', 'runtime.stop', 'runtime.replace'
));
ALTER TABLE workflow_outbox ADD CONSTRAINT workflow_outbox_operation_kind_fkey
    FOREIGN KEY (operation_id, kind) REFERENCES operations (id, type);

-- +goose Down
ALTER TABLE workflow_outbox DROP CONSTRAINT workflow_outbox_operation_kind_fkey;
DELETE FROM workflow_outbox WHERE kind IN ('runtime.start', 'runtime.stop', 'runtime.replace');
ALTER TABLE workflow_outbox DROP CONSTRAINT workflow_outbox_kind_check;
ALTER TABLE workflow_outbox ADD CONSTRAINT workflow_outbox_kind_check CHECK (kind = 'environment.create');
DROP TABLE runtime_operation_targets;
DELETE FROM operations WHERE type IN ('runtime.start', 'runtime.stop', 'runtime.replace');
ALTER TABLE operations DROP CONSTRAINT operations_id_environment_type_key;
ALTER TABLE operations DROP CONSTRAINT operations_id_type_key;
UPDATE environments SET current_runtime_id = NULL WHERE current_runtime_id IS NOT NULL;
ALTER TABLE environments DROP CONSTRAINT environments_current_runtime_fkey;
DROP TRIGGER runtimes_identity_guard ON runtimes;
DROP FUNCTION preserve_runtime_identity();
DROP TRIGGER runtimes_sequence_guard ON runtimes;
DROP FUNCTION enforce_runtime_sequence();
DROP TABLE runtimes;
ALTER TABLE environments DROP CONSTRAINT environments_runtime_placement_canonical;
ALTER TABLE environments DROP CONSTRAINT environments_runtime_placement_key;
DROP FUNCTION sshai_trim_space(TEXT);
