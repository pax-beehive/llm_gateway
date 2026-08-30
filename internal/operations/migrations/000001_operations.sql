CREATE TABLE IF NOT EXISTS operations_schema_metadata (
    component       text PRIMARY KEY,
    current_version integer NOT NULL CHECK (current_version > 0),
    updated_at      timestamptz NOT NULL
);

INSERT INTO operations_schema_metadata (component,current_version,updated_at)
VALUES ('control-plane',21,now())
ON CONFLICT (component) DO UPDATE SET
    current_version=GREATEST(operations_schema_metadata.current_version,EXCLUDED.current_version),
    updated_at=EXCLUDED.updated_at;

CREATE TABLE IF NOT EXISTS operations_gateway_heartbeats (
    gateway_id                  text PRIMARY KEY,
    region                      text NOT NULL,
    build_sha                   text NOT NULL,
    database_schema_version     integer NOT NULL CHECK (database_schema_version > 0),
    routing_catalog_revision    bigint NOT NULL CHECK (routing_catalog_revision >= 0),
    access_projection_revision  bigint NOT NULL CHECK (access_projection_revision >= 0),
    execution_epoch_floor       bigint NOT NULL CHECK (execution_epoch_floor >= 0),
    last_usage_outbox_id        bigint NOT NULL CHECK (last_usage_outbox_id >= 0),
    started_at                  timestamptz NOT NULL,
    observed_at                 timestamptz NOT NULL,
    received_at                 timestamptz NOT NULL,
    consumers                   jsonb NOT NULL,
    backlogs                    jsonb NOT NULL
);

CREATE TABLE IF NOT EXISTS operations_access_rollout_receipts (
    gateway_id          text NOT NULL,
    region              text NOT NULL,
    event_id            text NOT NULL,
    delivery_sequence   bigint NOT NULL CHECK (delivery_sequence > 0),
    aggregate_type      text NOT NULL CHECK (aggregate_type IN ('Tenant','GatewayAPIKey')),
    aggregate_id        text NOT NULL,
    aggregate_revision  bigint NOT NULL CHECK (aggregate_revision > 0),
    status              text NOT NULL CHECK (status IN ('applied','rejected')),
    error_code          text,
    occurred_at         timestamptz NOT NULL,
    observed_at         timestamptz NOT NULL,
    PRIMARY KEY (gateway_id,event_id,status),
    UNIQUE (gateway_id,delivery_sequence,status)
);

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='operations_access_rollout_receipts'::regclass
          AND conname='operations_access_rollout_receipts_pkey'
          AND array_length(conkey,1)=2
    ) THEN
        ALTER TABLE operations_access_rollout_receipts DROP CONSTRAINT operations_access_rollout_receipts_pkey;
        ALTER TABLE operations_access_rollout_receipts ADD PRIMARY KEY (gateway_id,event_id,status);
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS operations_access_receipts_delivery_status_idx
    ON operations_access_rollout_receipts (gateway_id,delivery_sequence,status);

CREATE INDEX IF NOT EXISTS operations_access_receipts_sequence_idx
    ON operations_access_rollout_receipts (gateway_id,delivery_sequence DESC);

CREATE INDEX IF NOT EXISTS operations_gateway_heartbeats_region_idx
    ON operations_gateway_heartbeats (region,gateway_id);
