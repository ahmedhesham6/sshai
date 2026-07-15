-- +goose Up
ALTER TABLE ssh_keys
    ADD CONSTRAINT ssh_keys_owner_id_key UNIQUE (owner_user_id, id);

CREATE TABLE environment_ssh_keys (
    environment_id TEXT NOT NULL,
    owner_user_id TEXT NOT NULL,
    ssh_key_id TEXT NOT NULL,
    PRIMARY KEY (environment_id, ssh_key_id),
    FOREIGN KEY (owner_user_id, environment_id)
        REFERENCES environments (owner_user_id, id) ON DELETE CASCADE,
    FOREIGN KEY (owner_user_id, ssh_key_id)
        REFERENCES ssh_keys (owner_user_id, id)
);

-- +goose Down
DROP TABLE environment_ssh_keys;
ALTER TABLE ssh_keys DROP CONSTRAINT ssh_keys_owner_id_key;
