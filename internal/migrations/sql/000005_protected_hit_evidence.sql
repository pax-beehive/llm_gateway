ALTER TABLE cache_leases
    ADD COLUMN IF NOT EXISTS original_expires_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_refresh_succeeded_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_refresh_expires_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_refresh_cost_micros bigint NOT NULL DEFAULT 0 CHECK (last_refresh_cost_micros >= 0),
    ADD COLUMN IF NOT EXISTS last_forecast_cost_micros bigint NOT NULL DEFAULT 0 CHECK (last_forecast_cost_micros >= 0),
    ADD COLUMN IF NOT EXISTS last_storage_cost_micros bigint NOT NULL DEFAULT 0 CHECK (last_storage_cost_micros >= 0),
    ADD COLUMN IF NOT EXISTS last_route_lock_cost_micros bigint NOT NULL DEFAULT 0 CHECK (last_route_lock_cost_micros >= 0);

UPDATE cache_leases
SET original_expires_at = estimated_expires_at
WHERE original_expires_at IS NULL;

ALTER TABLE cache_leases
    ALTER COLUMN original_expires_at SET NOT NULL;
