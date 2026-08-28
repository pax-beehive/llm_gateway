CREATE TABLE IF NOT EXISTS tenant_usage_daily (
    usage_date               date NOT NULL,
    tenant_id                text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    provider                 text NOT NULL,
    model                    text NOT NULL,
    currency                 text NOT NULL,
    response_count           bigint NOT NULL DEFAULT 0 CHECK (response_count >= 0),
    input_tokens             bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    cached_input_tokens      bigint NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
    cache_write_input_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_write_input_tokens >= 0),
    output_tokens            bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    amount_micros            bigint NOT NULL DEFAULT 0 CHECK (amount_micros >= 0),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (usage_date, tenant_id, provider, model, currency)
);

CREATE TABLE IF NOT EXISTS api_key_usage_daily (
    usage_date               date NOT NULL,
    tenant_id                text NOT NULL,
    api_key_id               text NOT NULL,
    provider                 text NOT NULL,
    model                    text NOT NULL,
    currency                 text NOT NULL,
    response_count           bigint NOT NULL DEFAULT 0 CHECK (response_count >= 0),
    input_tokens             bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    cached_input_tokens      bigint NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
    cache_write_input_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_write_input_tokens >= 0),
    output_tokens            bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    amount_micros            bigint NOT NULL DEFAULT 0 CHECK (amount_micros >= 0),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (usage_date, tenant_id, api_key_id, provider, model, currency),
    FOREIGN KEY (tenant_id, api_key_id)
        REFERENCES api_keys(tenant_id, id) ON DELETE CASCADE
);
