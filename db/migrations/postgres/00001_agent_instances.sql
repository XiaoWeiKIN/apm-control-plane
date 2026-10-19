-- +goose Up
CREATE TABLE agent_instances (
    tenant_id VARCHAR(96) NOT NULL,
    instance_uid VARCHAR(32) NOT NULL,
    credential_id VARCHAR(128) NOT NULL,
    agent_kind VARCHAR(64) NOT NULL DEFAULT 'unknown',
    distribution VARCHAR(128) NOT NULL DEFAULT '',
    distribution_version VARCHAR(64) NOT NULL DEFAULT '',
    service_name VARCHAR(255) NOT NULL DEFAULT '',
    service_namespace VARCHAR(255) NOT NULL DEFAULT '',
    environment VARCHAR(128) NOT NULL DEFAULT '',
    service_version VARCHAR(64) NOT NULL DEFAULT '',
    last_seen TIMESTAMPTZ NOT NULL,
    last_transport VARCHAR(16) NOT NULL,
    healthy BOOLEAN NULL,
    capabilities BIGINT NOT NULL DEFAULT 0,
    reported_hash VARCHAR(64) NOT NULL DEFAULT '',
    config_status VARCHAR(32) NOT NULL DEFAULT '',
    config_error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, instance_uid)
);
CREATE INDEX agent_instances_last_seen_idx ON agent_instances (tenant_id, last_seen);

-- +goose Down
DROP TABLE agent_instances;
