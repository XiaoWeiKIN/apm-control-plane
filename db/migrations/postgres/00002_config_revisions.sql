-- +goose Up
-- Immutable compiled OpAMP ConfigMap snapshots. No Policy FK until Policy is implemented.
CREATE TABLE config_revisions (
    workspace_id VARCHAR(96) NOT NULL,
    config_revision_id VARCHAR(36) NOT NULL,
    source_policy_revision_id VARCHAR(36) NULL,
    agent_kind VARCHAR(64) NOT NULL,
    distribution VARCHAR(128) NOT NULL DEFAULT '',
    config_dialect VARCHAR(96) NOT NULL,
    payload_format VARCHAR(64) NOT NULL,
    config_map_bytes BYTEA NOT NULL,
    config_hash CHAR(64) NOT NULL,
    hash_algorithm VARCHAR(32) NOT NULL,
    payload_size_bytes BIGINT NOT NULL CHECK (payload_size_bytes > 0),
    created_at TIMESTAMPTZ NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    PRIMARY KEY (workspace_id, config_revision_id)
);

-- +goose Down
DROP TABLE config_revisions;
