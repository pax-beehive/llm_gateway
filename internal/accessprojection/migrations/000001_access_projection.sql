CREATE TABLE IF NOT EXISTS gateway_access_projection (
    tenant_id              text NOT NULL,
    api_key_id             text NOT NULL,
    key_prefix             text NOT NULL,
    secret_digest          bytea NOT NULL,
    digest_version         smallint NOT NULL CHECK (digest_version > 0),
    key_status             text NOT NULL CHECK (key_status IN ('active', 'revoked')),
    key_revision           bigint NOT NULL CHECK (key_revision > 0),
    api_key_policy_revision bigint NOT NULL CHECK (api_key_policy_revision > 0),
    api_key_policy         jsonb NOT NULL,
    expires_at             timestamptz,
    revoked_at             timestamptz,
    last_used_at           timestamptz,
    tenant_status          text NOT NULL CHECK (tenant_status IN ('active', 'suspended', 'closed')),
    tenant_revision        bigint NOT NULL CHECK (tenant_revision > 0),
    home_region            text NOT NULL,
    execution_epoch        bigint NOT NULL CHECK (execution_epoch > 0),
    tenant_policy_revision bigint NOT NULL CHECK (tenant_policy_revision > 0),
    tenant_policy          jsonb NOT NULL,
    event_occurred_at      timestamptz NOT NULL,
    applied_at             timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, api_key_id),
    UNIQUE (secret_digest, digest_version)
);

CREATE INDEX IF NOT EXISTS gateway_access_projection_digest_idx
    ON gateway_access_projection (digest_version, secret_digest)
    WHERE key_status = 'active' AND tenant_status = 'active';

ALTER TABLE gateway_access_projection
    ADD COLUMN IF NOT EXISTS last_used_at timestamptz;

CREATE TABLE IF NOT EXISTS gateway_access_inbox (
    event_id           text PRIMARY KEY,
    delivery_sequence  bigint,
    schema_version     integer NOT NULL CHECK (schema_version > 0),
    aggregate_type     text NOT NULL,
    aggregate_id       text NOT NULL,
    aggregate_revision bigint NOT NULL CHECK (aggregate_revision > 0),
    event_type         text,
    event_occurred_at  timestamptz,
    apply_lag_seconds  double precision,
    disposition        text NOT NULL CHECK (disposition IN ('applied', 'stale')),
    received_at        timestamptz NOT NULL
);

ALTER TABLE gateway_access_inbox
    ADD COLUMN IF NOT EXISTS delivery_sequence bigint,
    ADD COLUMN IF NOT EXISTS event_type text,
    ADD COLUMN IF NOT EXISTS event_occurred_at timestamptz,
    ADD COLUMN IF NOT EXISTS apply_lag_seconds double precision;

CREATE TABLE IF NOT EXISTS gateway_access_heads (
    aggregate_type text NOT NULL,
    aggregate_id   text NOT NULL,
    revision       bigint NOT NULL CHECK (revision > 0),
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (aggregate_type, aggregate_id)
);

CREATE TABLE IF NOT EXISTS gateway_access_gaps (
    aggregate_type   text NOT NULL,
    aggregate_id     text NOT NULL,
    expected_revision bigint NOT NULL CHECK (expected_revision > 0),
    received_revision bigint NOT NULL CHECK (received_revision > expected_revision),
    detected_at      timestamptz NOT NULL,
    last_event_id    text NOT NULL,
    delivery_sequence bigint,
    event_occurred_at timestamptz,
    PRIMARY KEY (aggregate_type, aggregate_id)
);

ALTER TABLE gateway_access_gaps
    ADD COLUMN IF NOT EXISTS delivery_sequence bigint,
    ADD COLUMN IF NOT EXISTS event_occurred_at timestamptz;

CREATE INDEX IF NOT EXISTS gateway_access_inbox_delivery_sequence_idx
    ON gateway_access_inbox (delivery_sequence)
    WHERE delivery_sequence IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS gateway_access_inbox_delivery_sequence_unique_idx
    ON gateway_access_inbox (delivery_sequence)
    WHERE delivery_sequence IS NOT NULL;

CREATE TABLE IF NOT EXISTS gateway_access_rollout_receipts (
    event_id            text NOT NULL,
    delivery_sequence   bigint NOT NULL CHECK (delivery_sequence > 0),
    aggregate_type      text NOT NULL CHECK (aggregate_type IN ('Tenant','GatewayAPIKey')),
    aggregate_id        text NOT NULL,
    aggregate_revision  bigint NOT NULL CHECK (aggregate_revision > 0),
    status              text NOT NULL CHECK (status IN ('applied','rejected')),
    error_code          text,
    event_occurred_at   timestamptz NOT NULL,
    observed_at         timestamptz NOT NULL,
    reported_at         timestamptz,
    PRIMARY KEY (event_id,status),
    UNIQUE (delivery_sequence,status)
);

CREATE INDEX IF NOT EXISTS gateway_access_rollout_receipts_pending_idx
    ON gateway_access_rollout_receipts (observed_at,event_id,status)
    WHERE reported_at IS NULL;

CREATE INDEX IF NOT EXISTS gateway_access_gaps_detected_idx
    ON gateway_access_gaps (detected_at, aggregate_type, aggregate_id);

CREATE TABLE IF NOT EXISTS gateway_access_response_slots (
    api_key_id text NOT NULL,
    lease_id   text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (api_key_id, lease_id)
);

CREATE INDEX IF NOT EXISTS gateway_access_response_slots_expiry_idx
    ON gateway_access_response_slots (api_key_id, expires_at);
