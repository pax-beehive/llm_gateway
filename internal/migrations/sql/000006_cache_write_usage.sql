ALTER TABLE provider_price_snapshots
    ADD COLUMN IF NOT EXISTS cache_write_per_million numeric(24, 10) NOT NULL DEFAULT 0
        CHECK (cache_write_per_million >= 0);

ALTER TABLE usage_ledger
    ADD COLUMN IF NOT EXISTS cache_write_input_tokens bigint NOT NULL DEFAULT 0
        CHECK (cache_write_input_tokens >= 0);
