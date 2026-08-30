CREATE TABLE IF NOT EXISTS operations_metering_heartbeats (
    metering_id             text PRIMARY KEY,
    region                  text NOT NULL,
    projection_generation   bigint NOT NULL CHECK (projection_generation > 0),
    projection_cutoff       timestamptz NOT NULL,
    pending_events          bigint NOT NULL CHECK (pending_events >= 0),
    oldest_pending_at       timestamptz,
    poison_events           bigint NOT NULL CHECK (poison_events >= 0),
    queued_exports          bigint NOT NULL CHECK (queued_exports >= 0),
    started_at              timestamptz NOT NULL,
    observed_at             timestamptz NOT NULL,
    received_at             timestamptz NOT NULL,
    CHECK ((pending_events = 0 AND oldest_pending_at IS NULL) OR
           (pending_events > 0 AND oldest_pending_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS operations_metering_heartbeats_region_idx
    ON operations_metering_heartbeats (region,observed_at DESC,metering_id);

INSERT INTO operations_schema_metadata (component,current_version,updated_at)
VALUES ('control-plane',24,now())
ON CONFLICT (component) DO UPDATE SET
    current_version=GREATEST(operations_schema_metadata.current_version,EXCLUDED.current_version),
    updated_at=EXCLUDED.updated_at;
