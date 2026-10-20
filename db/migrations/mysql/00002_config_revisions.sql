-- +goose NO TRANSACTION
-- +goose Up
-- Immutable compiled OpAMP ConfigMap snapshots. No Policy FK until Policy is implemented.
CREATE TABLE config_revisions (
    workspace_id VARCHAR(96) NOT NULL,
    config_revision_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_policy_revision_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    agent_kind VARCHAR(64) NOT NULL,
    distribution VARCHAR(128) NOT NULL DEFAULT '',
    config_dialect VARCHAR(96) NOT NULL,
    payload_format VARCHAR(64) NOT NULL,
    config_map_bytes LONGBLOB NOT NULL,
    config_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    hash_algorithm VARCHAR(32) NOT NULL,
    payload_size_bytes BIGINT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    PRIMARY KEY (workspace_id, config_revision_id),
    CONSTRAINT config_revisions_positive_size CHECK (payload_size_bytes > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- +goose Down
DROP TABLE config_revisions;
