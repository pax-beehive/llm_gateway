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
