ALTER TABLE usage_ledger
    ADD COLUMN IF NOT EXISTS api_key_id text;

CREATE INDEX IF NOT EXISTS usage_ledger_api_key_created_idx
    ON usage_ledger (tenant_id, api_key_id, created_at)
    WHERE api_key_id IS NOT NULL;

DO $$
BEGIN
    ALTER TABLE usage_ledger ADD CONSTRAINT usage_ledger_api_key_fk
        FOREIGN KEY (tenant_id, api_key_id)
        REFERENCES api_keys(tenant_id, id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
