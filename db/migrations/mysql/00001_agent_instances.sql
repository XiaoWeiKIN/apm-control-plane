-- +goose NO TRANSACTION
-- +goose Up
CREATE TABLE agent_instances (
    workspace_id VARCHAR(96) NOT NULL,
    instance_uid CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    credential_id VARCHAR(128) NOT NULL,
    agent_kind VARCHAR(64) NOT NULL DEFAULT 'unknown',
    distribution VARCHAR(128) NOT NULL DEFAULT '',
    distribution_version VARCHAR(64) NOT NULL DEFAULT '',
    service_name VARCHAR(255) NOT NULL DEFAULT '',
    service_namespace VARCHAR(255) NOT NULL DEFAULT '',
    environment VARCHAR(128) NOT NULL DEFAULT '',
    service_version VARCHAR(64) NOT NULL DEFAULT '',
    last_seen DATETIME(6) NOT NULL,
    last_transport VARCHAR(16) NOT NULL,
    healthy BOOLEAN NULL,
    capabilities BIGINT NOT NULL DEFAULT 0,
    reported_hash VARCHAR(64) NOT NULL DEFAULT '',
    config_status VARCHAR(32) NOT NULL DEFAULT '',
    config_error TEXT NOT NULL,
    PRIMARY KEY (workspace_id, instance_uid),
    INDEX agent_instances_last_seen_idx (workspace_id, last_seen)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- +goose Down
DROP TABLE agent_instances;
