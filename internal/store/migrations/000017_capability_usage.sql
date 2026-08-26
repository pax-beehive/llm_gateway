ALTER TABLE provider_price_snapshots
    ADD COLUMN IF NOT EXISTS embedding_input_per_million bigint NOT NULL DEFAULT 0 CHECK (embedding_input_per_million >= 0),
    ADD COLUMN IF NOT EXISTS moderation_input_per_million bigint NOT NULL DEFAULT 0 CHECK (moderation_input_per_million >= 0),
    ADD COLUMN IF NOT EXISTS rerank_document_per_thousand bigint NOT NULL DEFAULT 0 CHECK (rerank_document_per_thousand >= 0);

CREATE TABLE IF NOT EXISTS capability_usage_ledger (
    id                       text PRIMARY KEY,
    tenant_id                text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    api_key_id               text,
    operation_id             text NOT NULL,
    home_region              text NOT NULL,
    execution_epoch          bigint NOT NULL CHECK (execution_epoch > 0),
    quota_reservation_id     text,
    capability               text NOT NULL CHECK (capability IN ('embeddings', 'moderation', 'rerank')),
    route_id                 text NOT NULL,
    provider                 text NOT NULL,
    model                    text NOT NULL,
    price_snapshot_id        text NOT NULL REFERENCES provider_price_snapshots(id),
    provider_usage           jsonb NOT NULL,
    input_units              bigint NOT NULL DEFAULT 0 CHECK (input_units >= 0),
    dimensions               bigint NOT NULL DEFAULT 0 CHECK (dimensions >= 0),
    documents                bigint NOT NULL DEFAULT 0 CHECK (documents >= 0),
    amount_micros            bigint NOT NULL DEFAULT 0 CHECK (amount_micros >= 0),
    currency                 text NOT NULL,
    created_at               timestamptz NOT NULL,
    UNIQUE (tenant_id, operation_id),
    FOREIGN KEY (tenant_id, api_key_id) REFERENCES api_keys(tenant_id, id),
    FOREIGN KEY (quota_reservation_id) REFERENCES quota_reservations(id)
);

ALTER TABLE capability_usage_ledger
    ADD COLUMN IF NOT EXISTS home_region text,
    ADD COLUMN IF NOT EXISTS execution_epoch bigint;

UPDATE capability_usage_ledger u
SET home_region = t.home_region, execution_epoch = t.execution_epoch
FROM tenants t
WHERE u.tenant_id = t.id AND (u.home_region IS NULL OR u.execution_epoch IS NULL);

ALTER TABLE capability_usage_ledger
    ALTER COLUMN home_region SET NOT NULL,
    ALTER COLUMN execution_epoch SET NOT NULL;

ALTER TABLE capability_usage_ledger DROP CONSTRAINT IF EXISTS capability_usage_ledger_execution_epoch_check;
ALTER TABLE capability_usage_ledger ADD CONSTRAINT capability_usage_ledger_execution_epoch_check CHECK (execution_epoch > 0);
ALTER TABLE capability_usage_ledger DROP CONSTRAINT IF EXISTS capability_usage_ledger_tenant_id_fkey;
ALTER TABLE capability_usage_ledger ADD CONSTRAINT capability_usage_ledger_tenant_id_fkey
    FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS capability_usage_tenant_created_idx
    ON capability_usage_ledger (tenant_id, capability, created_at);

CREATE INDEX IF NOT EXISTS capability_usage_api_key_created_idx
    ON capability_usage_ledger (tenant_id, api_key_id, capability, created_at)
    WHERE api_key_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS capability_usage_daily (
    usage_date       date NOT NULL,
    tenant_id        text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_key_id       text NOT NULL,
    capability       text NOT NULL CHECK (capability IN ('embeddings', 'moderation', 'rerank')),
    provider         text NOT NULL,
    model            text NOT NULL,
    currency         text NOT NULL,
    operation_count  bigint NOT NULL DEFAULT 0 CHECK (operation_count >= 0),
    input_units      bigint NOT NULL DEFAULT 0 CHECK (input_units >= 0),
    documents        bigint NOT NULL DEFAULT 0 CHECK (documents >= 0),
    amount_micros    bigint NOT NULL DEFAULT 0 CHECK (amount_micros >= 0),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (usage_date, tenant_id, api_key_id, capability, provider, model, currency),
    FOREIGN KEY (tenant_id, api_key_id) REFERENCES api_keys(tenant_id, id)
);
