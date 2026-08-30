CREATE TABLE IF NOT EXISTS gateway_control_event_offsets (
    stream_name text PRIMARY KEY,
    cursor          bigint NOT NULL CHECK (cursor >= 0),
    source_head     bigint NOT NULL DEFAULT 0 CHECK (source_head >= 0),
    last_fetched_at timestamptz,
    last_succeeded_at timestamptz,
    last_attempt_at timestamptz,
    failure_started_at timestamptz,
    last_error_code text,
    updated_at  timestamptz NOT NULL
);

ALTER TABLE gateway_control_event_offsets
    ADD COLUMN IF NOT EXISTS source_head bigint NOT NULL DEFAULT 0 CHECK (source_head >= 0),
    ADD COLUMN IF NOT EXISTS last_fetched_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_succeeded_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_attempt_at timestamptz,
    ADD COLUMN IF NOT EXISTS failure_started_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_error_code text;
