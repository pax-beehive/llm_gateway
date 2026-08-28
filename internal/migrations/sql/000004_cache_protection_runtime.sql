ALTER TABLE cache_leases
    ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS refresh_count integer NOT NULL DEFAULT 0 CHECK (refresh_count >= 0),
    ADD COLUMN IF NOT EXISTS spent_micros bigint NOT NULL DEFAULT 0 CHECK (spent_micros >= 0);

ALTER TABLE cache_refresh_intents
    ADD COLUMN IF NOT EXISTS candidate jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS error text;

CREATE INDEX IF NOT EXISTS cache_refresh_intents_due_idx
    ON cache_refresh_intents (scheduled_for, tenant_id, id)
    WHERE status = 'planned';
