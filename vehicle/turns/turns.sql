CREATE TABLE IF NOT EXISTS voice_turns (
    scope_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    voice_turn_id VARBINARY(128) NOT NULL,
    request_id VARBINARY(128) NULL,
    turn_handle VARBINARY(128) NULL,
    hermes_session_id VARBINARY(128) NULL,
    record_json JSON NOT NULL,
    closed BOOLEAN NOT NULL DEFAULT FALSE,
    cancelled BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (scope_key, voice_turn_id),
    UNIQUE KEY voice_turns_request (scope_key, request_id),
    UNIQUE KEY voice_turns_handle (turn_handle)
) ENGINE=InnoDB;
