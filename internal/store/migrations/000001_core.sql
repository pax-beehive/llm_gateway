CREATE TABLE IF NOT EXISTS tenants (
    id                  text PRIMARY KEY,
    home_region         text NOT NULL,
    policy_revision     bigint NOT NULL DEFAULT 1 CHECK (policy_revision > 0),
    policy              jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS conversations (
    tenant_id           text NOT NULL REFERENCES tenants(id),
    id                  text NOT NULL,
    home_region         text NOT NULL,
    revision            bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    execution_epoch     bigint NOT NULL DEFAULT 1 CHECK (execution_epoch > 0),
    metadata            jsonb NOT NULL DEFAULT '{}'::jsonb,
    deleted_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS responses (
    tenant_id           text NOT NULL REFERENCES tenants(id),
    id                  text NOT NULL,
    conversation_id     text,
    previous_response_id text,
    status              text NOT NULL,
    home_region         text NOT NULL,
    execution_epoch     bigint NOT NULL DEFAULT 1 CHECK (execution_epoch > 0),
    revision            bigint NOT NULL CHECK (revision > 0),
    payload             jsonb NOT NULL,
    deleted_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, conversation_id) REFERENCES conversations(tenant_id, id),
    FOREIGN KEY (tenant_id, previous_response_id) REFERENCES responses(tenant_id, id)
);

CREATE INDEX IF NOT EXISTS responses_conversation_created_idx
    ON responses (tenant_id, conversation_id, created_at, id)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS response_events (
    tenant_id           text NOT NULL,
    response_id         text NOT NULL,
    sequence_number     bigint NOT NULL CHECK (sequence_number > 0),
    event_type          text NOT NULL,
    payload             jsonb NOT NULL,
    expires_at          timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, response_id, sequence_number),
    FOREIGN KEY (tenant_id, response_id) REFERENCES responses(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           text NOT NULL REFERENCES tenants(id),
    operation           text NOT NULL,
    idempotency_key     text NOT NULL,
    request_hash        bytea NOT NULL,
    response_id         text NOT NULL,
    expires_at          timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, operation, idempotency_key),
    FOREIGN KEY (tenant_id, response_id) REFERENCES responses(tenant_id, id)
);

CREATE TABLE IF NOT EXISTS provider_price_snapshots (
    id                  text PRIMARY KEY,
    provider            text NOT NULL,
    model               text NOT NULL,
    region              text NOT NULL,
    currency            text NOT NULL,
    input_per_million   numeric(24, 10) NOT NULL CHECK (input_per_million >= 0),
    cached_input_per_million numeric(24, 10) CHECK (cached_input_per_million >= 0),
    output_per_million  numeric(24, 10) NOT NULL CHECK (output_per_million >= 0),
    effective_at        timestamptz NOT NULL,
    source              text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, model, region, effective_at)
);

CREATE TABLE IF NOT EXISTS usage_ledger (
    id                  text PRIMARY KEY,
    tenant_id           text NOT NULL REFERENCES tenants(id),
    response_id         text NOT NULL,
    attempt_id          text NOT NULL,
    price_snapshot_id   text NOT NULL REFERENCES provider_price_snapshots(id),
    provider_usage      jsonb NOT NULL,
    input_tokens        bigint NOT NULL CHECK (input_tokens >= 0),
    cached_input_tokens bigint NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
    output_tokens       bigint NOT NULL CHECK (output_tokens >= 0),
    amount              numeric(24, 10) NOT NULL CHECK (amount >= 0),
    currency            text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, response_id) REFERENCES responses(tenant_id, id),
    UNIQUE (tenant_id, response_id, attempt_id)
);

CREATE TABLE IF NOT EXISTS cache_leases (
    tenant_id           text NOT NULL REFERENCES tenants(id),
    id                  text NOT NULL,
    revision            bigint NOT NULL CHECK (revision > 0),
    route_id            text NOT NULL,
    provider            text NOT NULL,
    model               text NOT NULL,
    credential_scope    text NOT NULL,
    region              text NOT NULL,
    cache_key           text NOT NULL,
    prefix_hash         text NOT NULL,
    estimated_expires_at timestamptz NOT NULL,
    fencing_token       bigint NOT NULL CHECK (fencing_token > 0),
    status              text NOT NULL,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, provider, model, credential_scope, region, cache_key, prefix_hash)
);

CREATE TABLE IF NOT EXISTS cache_refresh_intents (
    tenant_id           text NOT NULL,
    id                  text NOT NULL,
    cache_lease_id      text NOT NULL,
    cache_lease_revision bigint NOT NULL,
    fencing_token       bigint NOT NULL,
    status              text NOT NULL,
    expected_net_saving numeric(24, 10) NOT NULL,
    scheduled_for       timestamptz NOT NULL,
    provider_result     jsonb,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, cache_lease_id) REFERENCES cache_leases(tenant_id, id),
    UNIQUE (tenant_id, cache_lease_id, cache_lease_revision)
);

CREATE TABLE IF NOT EXISTS savings_ledger (
    id                  text PRIMARY KEY,
    tenant_id           text NOT NULL REFERENCES tenants(id),
    response_id         text,
    cache_lease_id      text,
    measure             text NOT NULL CHECK (measure IN ('observed_discount', 'estimated_protected_saving', 'experimentally_validated_saving')),
    attribution         text NOT NULL CHECK (attribution IN ('observed', 'estimated', 'unavailable', 'experiment')),
    price_snapshot_id   text NOT NULL REFERENCES provider_price_snapshots(id),
    provider_usage      jsonb NOT NULL,
    gross_saving        numeric(24, 10) NOT NULL,
    refresh_cost        numeric(24, 10) NOT NULL DEFAULT 0,
    forecast_cost       numeric(24, 10) NOT NULL DEFAULT 0,
    storage_cost        numeric(24, 10) NOT NULL DEFAULT 0,
    route_lock_cost     numeric(24, 10) NOT NULL DEFAULT 0,
    net_saving          numeric(24, 10) NOT NULL,
    currency            text NOT NULL,
    holdout_cohort      text,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS transactional_outbox (
    id                  bigserial PRIMARY KEY,
    tenant_id           text NOT NULL REFERENCES tenants(id),
    aggregate_type      text NOT NULL,
    aggregate_id        text NOT NULL,
    aggregate_revision  bigint NOT NULL,
    event_type          text NOT NULL,
    payload             jsonb NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    published_at        timestamptz,
    UNIQUE (tenant_id, aggregate_type, aggregate_id, aggregate_revision, event_type)
);

CREATE INDEX IF NOT EXISTS transactional_outbox_unpublished_idx
    ON transactional_outbox (id) WHERE published_at IS NULL;
