-- +goose NO TRANSACTION
-- +goose Up
-- P2.2: one-Agent releases, frozen targets, current desired state, and audit.
CREATE TABLE config_releases (
    workspace_id VARCHAR(96) NOT NULL,
    release_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    policy_revision_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    agent_kind VARCHAR(64) NOT NULL,
    target_selector_json TEXT NOT NULL,
    request_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key VARCHAR(128) NULL,
    rollout_plan_json TEXT NOT NULL,
    status VARCHAR(24) NOT NULL,
    row_version BIGINT NOT NULL,
    rollback_of_release_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    requested_by VARCHAR(128) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    started_at DATETIME(6) NULL,
    finished_at DATETIME(6) NULL,
    PRIMARY KEY (workspace_id, release_id),
    UNIQUE KEY config_releases_idempotency_idx (workspace_id, idempotency_key),
    FOREIGN KEY (workspace_id, rollback_of_release_id)
        REFERENCES config_releases (workspace_id, release_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE release_targets (
    workspace_id VARCHAR(96) NOT NULL,
    release_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    instance_uid CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    config_revision_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    batch_no INTEGER NOT NULL DEFAULT 0,
    status VARCHAR(24) NOT NULL,
    desired_generation BIGINT NOT NULL,
    last_error TEXT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (workspace_id, release_id, instance_uid),
    INDEX release_targets_batch_idx (workspace_id, release_id, batch_no, status),
    INDEX release_targets_agent_idx (workspace_id, instance_uid),
    INDEX release_targets_config_idx (workspace_id, config_revision_id),
    FOREIGN KEY (workspace_id, release_id)
        REFERENCES config_releases (workspace_id, release_id),
    FOREIGN KEY (workspace_id, instance_uid)
        REFERENCES agent_instances (workspace_id, instance_uid),
    FOREIGN KEY (workspace_id, config_revision_id)
        REFERENCES config_revisions (workspace_id, config_revision_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE agent_assignments (
    workspace_id VARCHAR(96) NOT NULL,
    instance_uid CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    release_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    config_revision_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    desired_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    desired_generation BIGINT NOT NULL,
    row_version BIGINT NOT NULL,
    state VARCHAR(24) NOT NULL,
    observed_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    observed_status VARCHAR(32) NULL,
    observed_at DATETIME(6) NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_sent_at DATETIME(6) NULL,
    next_retry_at DATETIME(6) NULL,
    last_error TEXT NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (workspace_id, instance_uid),
    INDEX agent_assignments_retry_idx (workspace_id, state, next_retry_at),
    INDEX agent_assignments_release_idx (workspace_id, release_id),
    INDEX agent_assignments_config_idx (workspace_id, config_revision_id),
    INDEX agent_assignments_target_idx (workspace_id, release_id, instance_uid),
    FOREIGN KEY (workspace_id, release_id, instance_uid)
        REFERENCES release_targets (workspace_id, release_id, instance_uid),
    FOREIGN KEY (workspace_id, config_revision_id)
        REFERENCES config_revisions (workspace_id, config_revision_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE config_events (
    workspace_id VARCHAR(96) NOT NULL,
    event_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    event_type VARCHAR(48) NOT NULL,
    release_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    instance_uid CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
    desired_generation BIGINT NULL,
    config_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    reported_status VARCHAR(32) NULL,
    actor_type VARCHAR(24) NOT NULL,
    actor_id VARCHAR(128) NOT NULL,
    correlation_id VARCHAR(128) NULL,
    details_json TEXT NOT NULL,
    occurred_at DATETIME(6) NOT NULL,
    PRIMARY KEY (workspace_id, event_id),
    INDEX config_events_release_idx (workspace_id, release_id, occurred_at),
    INDEX config_events_agent_idx (workspace_id, instance_uid, occurred_at),
    FOREIGN KEY (workspace_id, release_id)
        REFERENCES config_releases (workspace_id, release_id),
    FOREIGN KEY (workspace_id, instance_uid)
        REFERENCES agent_instances (workspace_id, instance_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- +goose Down
DROP TABLE config_events;
DROP TABLE agent_assignments;
DROP TABLE release_targets;
DROP TABLE config_releases;
