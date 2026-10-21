-- +goose Up
-- P2.4: same business model as PostgreSQL, MySQL-compatible DDL.
CREATE TABLE policies (
    workspace_id VARCHAR(96) NOT NULL,
    policy_id VARCHAR(36) NOT NULL,
    policy_type VARCHAR(48) NOT NULL,
    agent_kind VARCHAR(64) NOT NULL,
    name VARCHAR(128) NOT NULL,
    status VARCHAR(24) NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (workspace_id, policy_id)
) ENGINE=InnoDB;

CREATE TABLE policy_revisions (
    workspace_id VARCHAR(96) NOT NULL,
    policy_revision_id VARCHAR(36) NOT NULL,
    policy_id VARCHAR(36) NOT NULL,
    revision_number BIGINT NOT NULL,
    schema_version INTEGER NOT NULL,
    body_json LONGTEXT NOT NULL,
    body_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (workspace_id, policy_revision_id),
    UNIQUE KEY policy_revision_number_unique (workspace_id, policy_id, revision_number),
    CONSTRAINT policy_revisions_policy_fk FOREIGN KEY (workspace_id, policy_id)
        REFERENCES policies (workspace_id, policy_id)
) ENGINE=InnoDB;

CREATE TABLE policy_bindings (
    workspace_id VARCHAR(96) NOT NULL,
    binding_id VARCHAR(36) NOT NULL,
    policy_revision_id VARCHAR(36) NOT NULL,
    agent_kind VARCHAR(64) NOT NULL,
    scope_type VARCHAR(24) NOT NULL,
    selector_json LONGTEXT NOT NULL,
    selector_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    priority INTEGER NOT NULL,
    enabled BOOLEAN NOT NULL,
    row_version BIGINT NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    updated_by VARCHAR(128) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (workspace_id, binding_id),
    INDEX policy_bindings_resolve_idx (workspace_id, agent_kind, enabled, scope_type, priority),
    CONSTRAINT policy_bindings_revision_fk FOREIGN KEY (workspace_id, policy_revision_id)
        REFERENCES policy_revisions (workspace_id, policy_revision_id)
) ENGINE=InnoDB;
ALTER TABLE config_revisions ADD CONSTRAINT config_revisions_source_policy_fk
    FOREIGN KEY (workspace_id, source_policy_revision_id)
    REFERENCES policy_revisions (workspace_id, policy_revision_id);

-- +goose Down
ALTER TABLE config_revisions DROP FOREIGN KEY config_revisions_source_policy_fk;
DROP TABLE policy_bindings;
DROP TABLE policy_revisions;
DROP TABLE policies;
