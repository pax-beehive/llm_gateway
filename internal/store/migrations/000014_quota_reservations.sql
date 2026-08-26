CREATE TABLE IF NOT EXISTS tenant_quota_counters (
    tenant_id        text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    metric           text NOT NULL,
    window_start     timestamptz NOT NULL,
    window_end       timestamptz NOT NULL,
    reserved_amount  bigint NOT NULL DEFAULT 0 CHECK (reserved_amount >= 0),
    committed_amount bigint NOT NULL DEFAULT 0 CHECK (committed_amount >= 0),
    revision         bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, metric, window_start),
    CHECK (window_end > window_start)
);

CREATE TABLE IF NOT EXISTS api_key_quota_counters (
    tenant_id        text NOT NULL,
    api_key_id       text NOT NULL,
    metric           text NOT NULL,
    window_start     timestamptz NOT NULL,
    window_end       timestamptz NOT NULL,
    reserved_amount  bigint NOT NULL DEFAULT 0 CHECK (reserved_amount >= 0),
    committed_amount bigint NOT NULL DEFAULT 0 CHECK (committed_amount >= 0),
    revision         bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, api_key_id, metric, window_start),
    FOREIGN KEY (tenant_id, api_key_id)
        REFERENCES api_keys(tenant_id, id) ON DELETE CASCADE,
    CHECK (window_end > window_start)
);

CREATE TABLE IF NOT EXISTS quota_reservations (
    id                       text PRIMARY KEY,
    tenant_id                text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_key_id               text NOT NULL,
    response_id              text NOT NULL,
    tenant_policy_revision   bigint NOT NULL CHECK (tenant_policy_revision > 0),
    api_key_policy_revision  bigint NOT NULL CHECK (api_key_policy_revision > 0),
    currency                 text NOT NULL,
    reserved_requests        bigint NOT NULL CHECK (reserved_requests >= 0),
    reserved_input_tokens    bigint NOT NULL CHECK (reserved_input_tokens >= 0),
    reserved_output_tokens   bigint NOT NULL CHECK (reserved_output_tokens >= 0),
    reserved_spend_micros    bigint NOT NULL CHECK (reserved_spend_micros >= 0),
    committed_requests       bigint NOT NULL DEFAULT 0 CHECK (committed_requests >= 0),
    committed_input_tokens   bigint NOT NULL DEFAULT 0 CHECK (committed_input_tokens >= 0),
    committed_output_tokens  bigint NOT NULL DEFAULT 0 CHECK (committed_output_tokens >= 0),
    committed_spend_micros   bigint NOT NULL DEFAULT 0 CHECK (committed_spend_micros >= 0),
    minute_window_start      timestamptz NOT NULL,
    day_window_start         timestamptz NOT NULL,
    month_window_start       timestamptz NOT NULL,
    status                   text NOT NULL
        CHECK (status IN ('reserved', 'committed', 'released', 'expired', 'uncertain')),
    expires_at               timestamptz NOT NULL,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, response_id),
    FOREIGN KEY (tenant_id, api_key_id)
        REFERENCES api_keys(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS quota_reservations_expiry_idx
    ON quota_reservations (expires_at, tenant_id, id)
    WHERE status = 'reserved';
