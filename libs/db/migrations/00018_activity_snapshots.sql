-- +goose Up
ALTER TABLE auto_stop_policies
    ADD COLUMN generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0);

CREATE TABLE activity_snapshots (
    runtime_id TEXT NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    environment_id TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    ssh_connections INTEGER NOT NULL CHECK (ssh_connections >= 0),
    ide_connections INTEGER NOT NULL CHECK (ide_connections >= 0),
    codex_processes INTEGER NOT NULL CHECK (codex_processes >= 0),
    claude_processes INTEGER NOT NULL CHECK (claude_processes >= 0),
    protected_processes INTEGER NOT NULL CHECK (protected_processes >= 0),
    selected_containers INTEGER NOT NULL CHECK (selected_containers >= 0),
    unknown_user_processes INTEGER NOT NULL CHECK (unknown_user_processes >= 0),
    PRIMARY KEY (runtime_id, sequence),
    FOREIGN KEY (environment_id, runtime_id) REFERENCES runtimes (environment_id, id)
);

CREATE INDEX activity_snapshots_latest_runtime_key
    ON activity_snapshots (runtime_id, sequence DESC);

-- +goose Down
DROP TABLE activity_snapshots;
ALTER TABLE auto_stop_policies DROP COLUMN generation;
