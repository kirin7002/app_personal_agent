CREATE TABLE IF NOT EXISTS vehicle_tasks (
    task_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    session_id VARCHAR(128) NOT NULL,
    vehicle_id VARCHAR(128) NOT NULL,
    device VARCHAR(64) NOT NULL,
    tool VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    known_params_json JSON NOT NULL,
    missing_fields_json JSON NOT NULL,
    question VARCHAR(512) NOT NULL DEFAULT '',
    command_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    last_request_id VARBINARY(128) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (task_id),
    UNIQUE KEY vehicle_tasks_command (command_id),
    KEY vehicle_tasks_pending_idx (scope_key, status, expires_at, created_at, task_id),
    KEY vehicle_tasks_expiry_idx (status, expires_at)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS vehicle_turn_writes (
    scope_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id VARBINARY(128) NOT NULL,
    operation_kind VARCHAR(32) NOT NULL,
    fingerprint CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    task_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    command_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    result_json JSON NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (scope_key, request_id),
    KEY vehicle_turn_writes_task_idx (task_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS vehicle_command_outbox (
    command_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    task_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    session_id VARCHAR(128) NOT NULL,
    vehicle_id VARCHAR(128) NOT NULL,
    request_id VARBINARY(128) NOT NULL,
    device VARCHAR(64) NOT NULL,
    tool VARCHAR(64) NOT NULL,
    params_json JSON NOT NULL,
    status VARCHAR(32) NOT NULL,
    attempts INT UNSIGNED NOT NULL DEFAULT 0,
    downstream_id VARCHAR(128) NOT NULL DEFAULT '',
    lease_owner VARCHAR(128) NOT NULL DEFAULT '',
    lease_until DATETIME(6) NULL,
    dispatched_at DATETIME(6) NULL,
    last_error VARCHAR(512) NOT NULL DEFAULT '',
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (command_id),
    UNIQUE KEY vehicle_command_outbox_task (task_id),
    KEY vehicle_command_outbox_claim_idx (status, lease_until, created_at)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS vehicle_result_events (
    event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    session_id VARCHAR(128) NOT NULL,
    vehicle_id VARCHAR(128) NOT NULL,
    device VARCHAR(64) NOT NULL,
    tool VARCHAR(64) NOT NULL,
    request_id VARBINARY(128) NOT NULL,
    task_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    command_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(32) NOT NULL,
    occurred_at DATETIME(6) NOT NULL,
    PRIMARY KEY (event_id),
    KEY vehicle_result_events_scope_idx (scope_key, occurred_at, event_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS vehicle_playback_receipts (
    scope_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    playback_id VARBINARY(128) NOT NULL,
    settled_at DATETIME(6) NOT NULL,
    PRIMARY KEY (scope_key, playback_id),
    KEY vehicle_playback_receipts_expiration (settled_at)
) ENGINE=InnoDB;
