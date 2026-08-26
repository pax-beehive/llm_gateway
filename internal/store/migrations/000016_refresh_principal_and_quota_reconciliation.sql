ALTER TABLE cache_refresh_intents
    ADD COLUMN IF NOT EXISTS sponsor_api_key_id text;

ALTER TABLE cache_refresh_usage_ledger
    ADD COLUMN IF NOT EXISTS api_key_id text;

DO $$
BEGIN
    ALTER TABLE cache_refresh_intents ADD CONSTRAINT cache_refresh_intents_sponsor_api_key_fk
        FOREIGN KEY (tenant_id, sponsor_api_key_id)
        REFERENCES api_keys(tenant_id, id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE cache_refresh_usage_ledger ADD CONSTRAINT cache_refresh_usage_api_key_fk
        FOREIGN KEY (tenant_id, api_key_id)
        REFERENCES api_keys(tenant_id, id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS cache_refresh_intents_sponsor_idx
    ON cache_refresh_intents (tenant_id, sponsor_api_key_id, status, scheduled_for)
    WHERE sponsor_api_key_id IS NOT NULL;

ALTER TABLE quota_reservations
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'response',
    ADD COLUMN IF NOT EXISTS cache_refresh_intent_id text,
    ADD COLUMN IF NOT EXISTS uncertain_at timestamptz;

ALTER TABLE quota_reservations ALTER COLUMN response_id DROP NOT NULL;

DO $$
BEGIN
    ALTER TABLE quota_reservations ADD CONSTRAINT quota_reservations_kind_check
        CHECK (kind IN ('response', 'cache_refresh'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE quota_reservations ADD CONSTRAINT quota_reservations_identity_check CHECK (
        (kind = 'response' AND response_id IS NOT NULL AND cache_refresh_intent_id IS NULL)
        OR (kind = 'cache_refresh' AND response_id IS NULL AND cache_refresh_intent_id IS NOT NULL)
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE quota_reservations ADD CONSTRAINT quota_reservations_refresh_intent_fk
        FOREIGN KEY (tenant_id, cache_refresh_intent_id)
        REFERENCES cache_refresh_intents(tenant_id, id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS quota_reservations_refresh_intent_unique_idx
    ON quota_reservations (tenant_id, cache_refresh_intent_id)
    WHERE cache_refresh_intent_id IS NOT NULL;

ALTER TABLE usage_ledger
    ADD COLUMN IF NOT EXISTS quota_reservation_id text;

DO $$
BEGIN
    ALTER TABLE usage_ledger ADD CONSTRAINT usage_ledger_quota_reservation_fk
        FOREIGN KEY (quota_reservation_id) REFERENCES quota_reservations(id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS api_key_refresh_usage_daily (
    usage_date                    date NOT NULL,
    tenant_id                     text NOT NULL,
    api_key_id                    text NOT NULL,
    provider                      text NOT NULL,
    model                         text NOT NULL,
    currency                      text NOT NULL,
    refresh_count                 bigint NOT NULL DEFAULT 0 CHECK (refresh_count >= 0),
    input_tokens                  bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    cached_input_tokens           bigint NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
    cache_write_input_tokens      bigint NOT NULL DEFAULT 0 CHECK (cache_write_input_tokens >= 0),
    output_tokens                 bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    amount_micros                 bigint NOT NULL DEFAULT 0 CHECK (amount_micros >= 0),
    updated_at                    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (usage_date, tenant_id, api_key_id, provider, model, currency),
    FOREIGN KEY (tenant_id, api_key_id)
        REFERENCES api_keys(tenant_id, id) ON DELETE CASCADE
);
