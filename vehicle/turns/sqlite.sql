CREATE TABLE IF NOT EXISTS voice_turns (
    scope_key TEXT NOT NULL,
    voice_turn_id TEXT NOT NULL,
    request_id TEXT NULL,
    turn_handle TEXT NULL,
    hermes_session_id TEXT NULL,
    record_json TEXT NOT NULL,
    closed INTEGER NOT NULL DEFAULT 0,
    cancelled INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope_key, voice_turn_id),
    UNIQUE (scope_key, request_id),
    UNIQUE (turn_handle)
);

CREATE TABLE IF NOT EXISTS voice_scope_queue (
    ticket_id INTEGER PRIMARY KEY AUTOINCREMENT,
    scope_key TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS voice_scope_queue_order
    ON voice_scope_queue (scope_key, ticket_id);

CREATE INDEX IF NOT EXISTS voice_scope_queue_expiration
    ON voice_scope_queue (expires_at);
