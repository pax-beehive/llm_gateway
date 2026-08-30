CREATE TABLE IF NOT EXISTS metering_inbox (
    event_id          text PRIMARY KEY,
    source_outbox_id  bigint UNIQUE,
    schema_version    integer NOT NULL,
    event_type        text NOT NULL,
    tenant_id         text NOT NULL,
    occurred_at       timestamptz NOT NULL,
    payload           jsonb NOT NULL,
    consumed_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS metering_usage_facts (
    event_id                 text PRIMARY KEY REFERENCES metering_inbox(event_id),
    usage_id                 text NOT NULL,
    tenant_id                text NOT NULL,
    api_key_id               text,
    response_id              text,
    attempt_id               text,
    operation_id             text,
    capability               text,
    route_id                 text,
    provider                 text NOT NULL,
    public_model             text NOT NULL,
    provider_model           text NOT NULL,
    region                   text NOT NULL,
    price_snapshot_id        text NOT NULL,
    input_tokens             bigint NOT NULL,
    cached_input_tokens      bigint NOT NULL,
    cache_write_input_tokens bigint NOT NULL,
    output_tokens            bigint NOT NULL,
    input_units              bigint NOT NULL,
    documents                bigint NOT NULL,
    amount_micros            bigint NOT NULL,
    currency                 text NOT NULL,
    outcome                  text NOT NULL,
    corrects_event_id        text REFERENCES metering_usage_facts(event_id),
    reason                   text,
    occurred_at              timestamptz NOT NULL,
    UNIQUE (tenant_id, usage_id, event_id)
);

CREATE INDEX IF NOT EXISTS metering_facts_tenant_time_idx
    ON metering_usage_facts (tenant_id, occurred_at, event_id);
CREATE INDEX IF NOT EXISTS metering_facts_key_time_idx
    ON metering_usage_facts (tenant_id, api_key_id, occurred_at, event_id)
    WHERE api_key_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS metering_facts_response_idx
    ON metering_usage_facts (tenant_id, response_id, occurred_at, event_id)
    WHERE response_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS metering_projection_generations (
    id            bigserial PRIMARY KEY,
    status        text NOT NULL CHECK (status IN ('building','active','superseded','failed')),
    source_cutoff timestamptz NOT NULL,
    started_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz,
    error_code    text
);

CREATE UNIQUE INDEX IF NOT EXISTS metering_one_active_generation_idx
    ON metering_projection_generations ((status)) WHERE status='active';

CREATE TABLE IF NOT EXISTS metering_usage_daily (
    generation_id           bigint NOT NULL REFERENCES metering_projection_generations(id) ON DELETE CASCADE,
    usage_date              date NOT NULL,
    tenant_id               text NOT NULL,
    api_key_id              text NOT NULL DEFAULT '',
    provider                text NOT NULL,
    public_model            text NOT NULL,
    provider_model          text NOT NULL,
    route_id                text NOT NULL DEFAULT '',
    outcome                 text NOT NULL,
    currency                text NOT NULL,
    operation_count         bigint NOT NULL,
    input_tokens            bigint NOT NULL,
    cached_input_tokens     bigint NOT NULL,
    cache_write_input_tokens bigint NOT NULL,
    output_tokens           bigint NOT NULL,
    input_units             bigint NOT NULL,
    documents               bigint NOT NULL,
    amount_micros           bigint NOT NULL,
    PRIMARY KEY (generation_id,usage_date,tenant_id,api_key_id,provider,public_model,provider_model,route_id,outcome,currency)
);

CREATE TABLE IF NOT EXISTS metering_outbox_claims (
    source_outbox_id bigint PRIMARY KEY,
    worker_id        text NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    attempt_count    integer NOT NULL DEFAULT 1,
    error_code       text,
    poisoned         boolean NOT NULL DEFAULT false,
    updated_at       timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE metering_outbox_claims ADD COLUMN IF NOT EXISTS poisoned boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS metering_exports (
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL,
    status       text NOT NULL CHECK (status IN ('queued','running','succeeded','failed')),
    filter       jsonb NOT NULL,
    cutoff       timestamptz NOT NULL,
    object_key   text,
    sha256       text,
    row_count    bigint NOT NULL DEFAULT 0,
    error_code   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE INDEX IF NOT EXISTS metering_exports_tenant_created_idx
    ON metering_exports (tenant_id, created_at DESC, id DESC);

INSERT INTO metering_projection_generations(status,source_cutoff,completed_at)
SELECT 'active','epoch'::timestamptz,now()
WHERE NOT EXISTS (SELECT 1 FROM metering_projection_generations WHERE status='active');
