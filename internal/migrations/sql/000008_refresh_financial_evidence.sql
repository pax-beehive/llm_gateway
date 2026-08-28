CREATE TABLE IF NOT EXISTS cache_refresh_usage_ledger (
    id                       text PRIMARY KEY,
    tenant_id                text NOT NULL REFERENCES tenants(id),
    cache_refresh_intent_id  text NOT NULL,
    cache_lease_id           text NOT NULL,
    price_snapshot_id        text NOT NULL REFERENCES provider_price_snapshots(id),
    provider_usage           jsonb NOT NULL,
    input_tokens             bigint NOT NULL CHECK (input_tokens >= 0),
    cached_input_tokens      bigint NOT NULL CHECK (cached_input_tokens >= 0),
    cache_write_input_tokens bigint NOT NULL CHECK (cache_write_input_tokens >= 0),
    output_tokens            bigint NOT NULL CHECK (output_tokens >= 0),
    amount                   numeric(24, 10) NOT NULL CHECK (amount >= 0),
    currency                 text NOT NULL,
    created_at               timestamptz NOT NULL,
    UNIQUE (tenant_id, cache_refresh_intent_id),
    FOREIGN KEY (tenant_id, cache_refresh_intent_id)
        REFERENCES cache_refresh_intents(tenant_id, id),
    FOREIGN KEY (tenant_id, cache_lease_id)
        REFERENCES cache_leases(tenant_id, id)
);

ALTER TABLE cache_leases
    ADD COLUMN IF NOT EXISTS last_refresh_usage_id text,
    ADD COLUMN IF NOT EXISTS last_refresh_provider_usage jsonb;

ALTER TABLE savings_ledger
    ADD COLUMN IF NOT EXISTS refresh_usage_id text,
    ADD COLUMN IF NOT EXISTS refresh_provider_usage jsonb;
