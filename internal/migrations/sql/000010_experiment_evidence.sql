ALTER TABLE savings_ledger
    ADD COLUMN IF NOT EXISTS experiment_revision text;

ALTER TABLE cache_refresh_usage_ledger
    ADD COLUMN IF NOT EXISTS usage_reliable boolean NOT NULL DEFAULT false;
