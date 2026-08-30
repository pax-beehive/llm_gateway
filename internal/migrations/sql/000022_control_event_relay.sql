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

CREATE TABLE IF NOT EXISTS gateway_provider_connection_projection (
    id                       text PRIMARY KEY,
    provider                 text NOT NULL,
    base_url                 text NOT NULL,
    region                   text NOT NULL,
    credential_scope         text NOT NULL,
    capability_declaration   jsonb NOT NULL,
    administrative_status    text NOT NULL CHECK (administrative_status IN ('enabled','disabled')),
    revision                 bigint NOT NULL CHECK (revision > 0),
    credential_version       bigint NOT NULL CHECK (credential_version > 0),
    observed_healthy         boolean,
    event_occurred_at        timestamptz NOT NULL,
    applied_at               timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS gateway_provider_connection_projection_inbox (
    event_id                 text PRIMARY KEY,
    delivery_sequence        bigint NOT NULL UNIQUE CHECK (delivery_sequence > 0),
    aggregate_type           text NOT NULL,
    aggregate_id             text NOT NULL,
    aggregate_revision       bigint NOT NULL CHECK (aggregate_revision > 0),
    disposition              text NOT NULL CHECK (disposition IN ('applied','stale')),
    received_at              timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS gateway_provider_connection_projection_gaps (
    connection_id            text PRIMARY KEY,
    expected_revision        bigint NOT NULL CHECK (expected_revision > 0),
    received_revision        bigint NOT NULL CHECK (received_revision > expected_revision),
    event_id                 text NOT NULL,
    delivery_sequence        bigint NOT NULL,
    detected_at              timestamptz NOT NULL
);

UPDATE gateway_schema_metadata SET current_version=22,updated_at=now()
WHERE component='gateway' AND current_version<22;
