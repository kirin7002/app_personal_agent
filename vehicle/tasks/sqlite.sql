CREATE TABLE IF NOT EXISTS vehicle_tasks (
    task_id TEXT NOT NULL PRIMARY KEY,
    scope_key TEXT NOT NULL,
    user_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    device TEXT NOT NULL,
    tool TEXT NOT NULL,
    status TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    known_params_json TEXT NOT NULL,
    missing_fields_json TEXT NOT NULL,
    question TEXT NOT NULL DEFAULT '',
    command_id TEXT NULL UNIQUE,
    last_request_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS vehicle_tasks_pending_idx
    ON vehicle_tasks (scope_key, status, expires_at, created_at, task_id);

CREATE INDEX IF NOT EXISTS vehicle_tasks_expiry_idx
    ON vehicle_tasks (status, expires_at);

CREATE TABLE IF NOT EXISTS vehicle_turn_writes (
    scope_key TEXT NOT NULL,
    request_id TEXT NOT NULL,
    operation_kind TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    task_id TEXT NOT NULL,
    command_id TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (scope_key, request_id)
);

CREATE INDEX IF NOT EXISTS vehicle_turn_writes_task_idx
    ON vehicle_turn_writes (task_id);

CREATE TABLE IF NOT EXISTS vehicle_command_outbox (
    command_id TEXT NOT NULL PRIMARY KEY,
    task_id TEXT NOT NULL UNIQUE,
    scope_key TEXT NOT NULL,
    user_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    device TEXT NOT NULL,
    tool TEXT NOT NULL,
    params_json TEXT NOT NULL,
    status TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    downstream_id TEXT NOT NULL DEFAULT '',
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_until TEXT NULL,
    dispatched_at TEXT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS vehicle_command_outbox_claim_idx
    ON vehicle_command_outbox (status, lease_until, created_at);

CREATE TABLE IF NOT EXISTS vehicle_result_events (
    event_id TEXT NOT NULL PRIMARY KEY,
    scope_key TEXT NOT NULL,
    user_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    device TEXT NOT NULL,
    tool TEXT NOT NULL,
    request_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    command_id TEXT NOT NULL,
    status TEXT NOT NULL,
    occurred_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS vehicle_result_events_scope_idx
    ON vehicle_result_events (scope_key, occurred_at, event_id);

CREATE TABLE IF NOT EXISTS vehicle_playback_receipts (
    scope_key TEXT NOT NULL,
    playback_id TEXT NOT NULL,
    settled_at TEXT NOT NULL,
    PRIMARY KEY (scope_key, playback_id)
);

CREATE INDEX IF NOT EXISTS vehicle_playback_receipts_expiration
    ON vehicle_playback_receipts (settled_at);
