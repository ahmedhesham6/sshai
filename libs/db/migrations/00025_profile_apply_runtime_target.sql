-- +goose Up
ALTER TABLE runtime_operation_targets DROP CONSTRAINT runtime_operation_targets_operation_type_check;
ALTER TABLE runtime_operation_targets ADD CONSTRAINT runtime_operation_targets_operation_type_check
    CHECK (operation_type IN ('runtime.start', 'runtime.stop', 'runtime.replace', 'profile.apply'));

ALTER TABLE workflow_outbox DROP CONSTRAINT workflow_outbox_kind_check;
ALTER TABLE workflow_outbox ADD CONSTRAINT workflow_outbox_kind_check CHECK (kind IN (
    'environment.create', 'runtime.start', 'runtime.stop', 'runtime.replace', 'profile.apply'
));

-- +goose Down
DELETE FROM workflow_outbox WHERE kind = 'profile.apply';
DELETE FROM runtime_operation_targets WHERE operation_type = 'profile.apply';
DELETE FROM operations WHERE type = 'profile.apply';

ALTER TABLE workflow_outbox DROP CONSTRAINT workflow_outbox_kind_check;
ALTER TABLE workflow_outbox ADD CONSTRAINT workflow_outbox_kind_check CHECK (kind IN (
    'environment.create', 'runtime.start', 'runtime.stop', 'runtime.replace'
));

ALTER TABLE runtime_operation_targets DROP CONSTRAINT runtime_operation_targets_operation_type_check;
ALTER TABLE runtime_operation_targets ADD CONSTRAINT runtime_operation_targets_operation_type_check
    CHECK (operation_type IN ('runtime.start', 'runtime.stop', 'runtime.replace'));
