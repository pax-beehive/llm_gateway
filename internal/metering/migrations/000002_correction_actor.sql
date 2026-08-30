ALTER TABLE metering_usage_facts
    ADD COLUMN IF NOT EXISTS correction_actor_id text;

ALTER TABLE metering_exports
    ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz,
    ADD COLUMN IF NOT EXISTS attempt_count integer NOT NULL DEFAULT 0;
