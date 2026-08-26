ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS predecessor_id text,
    ADD COLUMN IF NOT EXISTS replacement_id text,
    ADD COLUMN IF NOT EXISTS grace_expires_at timestamptz;

DO $$
BEGIN
    ALTER TABLE api_keys ADD CONSTRAINT api_keys_predecessor_fk
        FOREIGN KEY (tenant_id, predecessor_id)
        REFERENCES api_keys(tenant_id, id) DEFERRABLE INITIALLY DEFERRED;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE api_keys ADD CONSTRAINT api_keys_replacement_fk
        FOREIGN KEY (tenant_id, replacement_id)
        REFERENCES api_keys(tenant_id, id) DEFERRABLE INITIALLY DEFERRED;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS api_keys_one_replacement_idx
    ON api_keys (tenant_id, predecessor_id) WHERE predecessor_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS api_keys_grace_reconciliation_idx
    ON api_keys (grace_expires_at, tenant_id, id)
    WHERE status = 'active' AND grace_expires_at IS NOT NULL;

CREATE OR REPLACE FUNCTION reject_api_key_policy_revision_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1 THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'Gateway API Key Policy revisions are append-only';
END $$;

DROP TRIGGER IF EXISTS api_key_policy_revisions_append_only_trigger ON api_key_policy_revisions;
CREATE TRIGGER api_key_policy_revisions_append_only_trigger
    BEFORE UPDATE OR DELETE ON api_key_policy_revisions
    FOR EACH ROW EXECUTE FUNCTION reject_api_key_policy_revision_mutation();
