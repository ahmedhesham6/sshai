-- +goose Up
ALTER TABLE operations DROP CONSTRAINT operations_check2;
ALTER TABLE operations ADD CONSTRAINT operations_succeeded_execution_check CHECK (
    status <> 'succeeded'
    OR restate_invocation_id IS NOT NULL
    OR type = 'environment.update_auto_stop'
);

-- +goose Down
ALTER TABLE operations DROP CONSTRAINT operations_succeeded_execution_check;
ALTER TABLE operations ADD CONSTRAINT operations_check2 CHECK (
    status <> 'succeeded' OR restate_invocation_id IS NOT NULL
);
