ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS execution_epoch bigint NOT NULL DEFAULT 1 CHECK (execution_epoch > 0);

ALTER TABLE responses
    ADD COLUMN IF NOT EXISTS retain_content boolean NOT NULL DEFAULT true;
