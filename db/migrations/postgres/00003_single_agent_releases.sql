-- +goose Up
-- P2.2: one-Agent releases, frozen targets, current desired state, and audit.
CREATE TABLE config_releases (
    workspace_id VARCHAR(96) NOT NULL,
    release_id VARCHAR(36) NOT NULL,
    policy_revision_id VARCHAR(36) NULL,
    agent_kind VARCHAR(64) NOT NULL,
    target_selector_json TEXT NOT NULL,
    request_sha256 CHAR(64) NOT NULL,
    idempotency_key VARCHAR(128) NULL,
    rollout_plan_json TEXT NOT NULL,
    status VARCHAR(24) NOT NULL,
    row_version BIGINT NOT NULL,
    rollback_of_release_id VARCHAR(36) NULL,
    requested_by VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ NULL,
    finished_at TIMESTAMPTZ NULL,
    PRIMARY KEY (workspace_id, release_id),
    UNIQUE (workspace_id, idempotency_key),
    FOREIGN KEY (workspace_id, rollback_of_release_id)
        REFERENCES config_releases (workspace_id, release_id)
);

CREATE TABLE release_targets (
    workspace_id VARCHAR(96) NOT NULL,
    release_id VARCHAR(36) NOT NULL,
    instance_uid VARCHAR(32) NOT NULL,
    config_revision_id VARCHAR(36) NOT NULL,
    batch_no INTEGER NOT NULL DEFAULT 0,
    status VARCHAR(24) NOT NULL,
    desired_generation BIGINT NOT NULL,
    last_error TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id, release_id, instance_uid),
    FOREIGN KEY (workspace_id, release_id)
        REFERENCES config_releases (workspace_id, release_id),
    FOREIGN KEY (workspace_id, instance_uid)
        REFERENCES agent_instances (workspace_id, instance_uid),
    FOREIGN KEY (workspace_id, config_revision_id)
        REFERENCES config_revisions (workspace_id, config_revision_id)
);
CREATE INDEX release_targets_batch_idx
    ON release_targets (workspace_id, release_id, batch_no, status);

CREATE TABLE agent_assignments (
    workspace_id VARCHAR(96) NOT NULL,
    instance_uid VARCHAR(32) NOT NULL,
    release_id VARCHAR(36) NOT NULL,
    config_revision_id VARCHAR(36) NOT NULL,
    desired_hash CHAR(64) NOT NULL,
    desired_generation BIGINT NOT NULL,
    row_version BIGINT NOT NULL,
    state VARCHAR(24) NOT NULL,
    observed_hash CHAR(64) NULL,
    observed_status VARCHAR(32) NULL,
    observed_at TIMESTAMPTZ NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_sent_at TIMESTAMPTZ NULL,
    next_retry_at TIMESTAMPTZ NULL,
    last_error TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id, instance_uid),
    FOREIGN KEY (workspace_id, release_id, instance_uid)
        REFERENCES release_targets (workspace_id, release_id, instance_uid),
    FOREIGN KEY (workspace_id, config_revision_id)
        REFERENCES config_revisions (workspace_id, config_revision_id)
);
CREATE INDEX agent_assignments_retry_idx
    ON agent_assignments (workspace_id, state, next_retry_at);
CREATE INDEX agent_assignments_release_idx
    ON agent_assignments (workspace_id, release_id);

CREATE TABLE config_events (
    workspace_id VARCHAR(96) NOT NULL,
    event_id VARCHAR(36) NOT NULL,
    event_type VARCHAR(48) NOT NULL,
    release_id VARCHAR(36) NULL,
    instance_uid VARCHAR(32) NULL,
    desired_generation BIGINT NULL,
    config_hash CHAR(64) NULL,
    reported_status VARCHAR(32) NULL,
    actor_type VARCHAR(24) NOT NULL,
    actor_id VARCHAR(128) NOT NULL,
    correlation_id VARCHAR(128) NULL,
    details_json TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id, event_id),
    FOREIGN KEY (workspace_id, release_id)
        REFERENCES config_releases (workspace_id, release_id),
    FOREIGN KEY (workspace_id, instance_uid)
        REFERENCES agent_instances (workspace_id, instance_uid)
);
CREATE INDEX config_events_release_idx
    ON config_events (workspace_id, release_id, occurred_at);
CREATE INDEX config_events_agent_idx
    ON config_events (workspace_id, instance_uid, occurred_at);

-- +goose Down
DROP TABLE config_events;
DROP TABLE agent_assignments;
DROP TABLE release_targets;
DROP TABLE config_releases;
