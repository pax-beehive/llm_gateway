ALTER TABLE control_audit_events
    ALTER COLUMN tenant_id DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS aggregate_type text,
    ADD COLUMN IF NOT EXISTS aggregate_id text;

CREATE INDEX IF NOT EXISTS control_audit_aggregate_time_idx
    ON control_audit_events (aggregate_type, aggregate_id, occurred_at, event_id);

ALTER TABLE control_outbox ALTER COLUMN tenant_id DROP NOT NULL;

CREATE TABLE IF NOT EXISTS provider_connections (
    id                       text PRIMARY KEY,
    provider                 text NOT NULL CHECK (provider IN ('openai','deepseek','anthropic','gemini')),
    display_name             text NOT NULL,
    base_url                 text NOT NULL,
    region                   text NOT NULL,
    credential_scope         text NOT NULL,
    secret_ref               text NOT NULL,
    secret_external_version  text NOT NULL,
    credential_version       bigint NOT NULL DEFAULT 1 CHECK (credential_version > 0),
    administrative_status    text NOT NULL DEFAULT 'disabled' CHECK (administrative_status IN ('enabled','disabled')),
    capability_declaration   jsonb NOT NULL,
    revision                 bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS provider_connections_list_idx
    ON provider_connections (provider, region, administrative_status, id);

CREATE TABLE IF NOT EXISTS provider_connection_credential_versions (
    connection_id            text NOT NULL REFERENCES provider_connections(id) ON DELETE RESTRICT,
    version                  bigint NOT NULL CHECK (version > 0),
    secret_ref               text NOT NULL,
    secret_external_version  text NOT NULL,
    status                   text NOT NULL CHECK (status IN ('active','retired')),
    activated_at             timestamptz NOT NULL,
    retired_at               timestamptz,
    PRIMARY KEY (connection_id, version)
);

CREATE TABLE IF NOT EXISTS provider_operations (
    id                       text PRIMARY KEY,
    operation_type           text NOT NULL CHECK (operation_type IN ('probe','model_discovery','credential_rotation')),
    connection_id            text NOT NULL REFERENCES provider_connections(id) ON DELETE RESTRICT,
    expected_revision        bigint NOT NULL CHECK (expected_revision > 0),
    status                   text NOT NULL CHECK (status IN ('queued','running','succeeded','failed')),
    live_authorized          boolean NOT NULL DEFAULT false,
    pending_secret_ref       text,
    pending_secret_version   text,
    actor_type               text NOT NULL,
    actor_id                 text NOT NULL,
    acting_tenant_id         text,
    scopes                   text[] NOT NULL,
    request_id               text NOT NULL,
    reason                   text NOT NULL,
    result                   jsonb NOT NULL DEFAULT '{}'::jsonb,
    error_code               text,
    error_message            text,
    created_at               timestamptz NOT NULL,
    started_at               timestamptz,
    completed_at             timestamptz
);

ALTER TABLE provider_operations
    ADD COLUMN IF NOT EXISTS attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;

DROP INDEX IF EXISTS provider_operations_queue_idx;
CREATE INDEX provider_operations_queue_idx
    ON provider_operations (created_at, id) WHERE status IN ('queued','running');

CREATE TABLE IF NOT EXISTS provider_connection_health (
    connection_id        text PRIMARY KEY REFERENCES provider_connections(id) ON DELETE RESTRICT,
    observed_status      text NOT NULL CHECK (observed_status IN ('healthy','unhealthy')),
    error_code           text,
    operation_id         text NOT NULL REFERENCES provider_operations(id) ON DELETE RESTRICT,
    latency_milliseconds bigint NOT NULL CHECK (latency_milliseconds >= 0),
    observed_at          timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_model_observations (
    operation_id       text NOT NULL REFERENCES provider_operations(id) ON DELETE RESTRICT,
    connection_id      text NOT NULL REFERENCES provider_connections(id) ON DELETE RESTRICT,
    provider_model_id  text NOT NULL,
    owned_by           text NOT NULL DEFAULT '',
    capabilities       jsonb NOT NULL DEFAULT '{}'::jsonb,
    raw_response_hash  text NOT NULL,
    observed_at        timestamptz NOT NULL,
    PRIMARY KEY (operation_id, provider_model_id)
);

CREATE INDEX IF NOT EXISTS provider_model_observations_connection_time_idx
    ON provider_model_observations (connection_id, observed_at, provider_model_id);
