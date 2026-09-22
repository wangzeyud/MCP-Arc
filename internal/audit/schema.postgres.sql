CREATE TABLE IF NOT EXISTS calls (
    id          BIGSERIAL PRIMARY KEY,
    client_id   TEXT    NOT NULL,
    tool_name   TEXT    NOT NULL,
    params      TEXT,
    raw_params  TEXT,
    result      TEXT,
    error_msg   TEXT,
    latency_ms  BIGINT,
    replay_of   BIGINT,
    timestamp   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_calls_client ON calls(client_id);
CREATE INDEX IF NOT EXISTS idx_calls_tool   ON calls(tool_name);
CREATE INDEX IF NOT EXISTS idx_calls_time   ON calls(timestamp);
