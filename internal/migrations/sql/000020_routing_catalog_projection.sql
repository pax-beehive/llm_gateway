CREATE TABLE IF NOT EXISTS gateway_routing_catalog_history (
    revision            bigint PRIMARY KEY CHECK (revision > 0),
    publication_id      text NOT NULL,
    document            jsonb NOT NULL,
    validation_hash     text NOT NULL,
    applied_at          timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS gateway_routing_catalog_head (
    singleton           boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    revision            bigint NOT NULL REFERENCES gateway_routing_catalog_history(revision) ON DELETE RESTRICT,
    updated_at          timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS gateway_routing_catalog_inbox (
    gateway_id          text NOT NULL,
    region              text NOT NULL,
    event_id            text NOT NULL,
    catalog_revision    bigint NOT NULL,
    publication_id      text NOT NULL,
    status              text NOT NULL CHECK (status IN ('applied','rejected')),
    error_code          text,
    observed_at         timestamptz NOT NULL,
    PRIMARY KEY (gateway_id, event_id)
);

ALTER TABLE gateway_routing_catalog_inbox ADD COLUMN IF NOT EXISTS gateway_id text;
ALTER TABLE gateway_routing_catalog_inbox ADD COLUMN IF NOT EXISTS region text;
UPDATE gateway_routing_catalog_inbox SET gateway_id = 'legacy-gateway' WHERE gateway_id IS NULL;
UPDATE gateway_routing_catalog_inbox SET region = 'legacy-region' WHERE region IS NULL;
ALTER TABLE gateway_routing_catalog_inbox ALTER COLUMN gateway_id SET NOT NULL;
ALTER TABLE gateway_routing_catalog_inbox ALTER COLUMN region SET NOT NULL;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'gateway_routing_catalog_inbox'::regclass
          AND contype = 'p'
          AND conkey = ARRAY[
              (SELECT attnum FROM pg_attribute WHERE attrelid='gateway_routing_catalog_inbox'::regclass AND attname='gateway_id'),
              (SELECT attnum FROM pg_attribute WHERE attrelid='gateway_routing_catalog_inbox'::regclass AND attname='event_id')
          ]::smallint[]
    ) THEN
        ALTER TABLE gateway_routing_catalog_inbox DROP CONSTRAINT IF EXISTS gateway_routing_catalog_inbox_pkey;
        ALTER TABLE gateway_routing_catalog_inbox ADD PRIMARY KEY (gateway_id, event_id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS gateway_routing_catalog_inbox_revision_idx
    ON gateway_routing_catalog_inbox (catalog_revision, observed_at);

CREATE TABLE IF NOT EXISTS gateway_provider_connection_inbox (
    gateway_id          text NOT NULL,
    event_id            text NOT NULL,
    connection_id       text NOT NULL,
    connection_revision bigint NOT NULL,
    status              text NOT NULL CHECK (status IN ('applied','rejected','ignored')),
    error_code          text,
    observed_at         timestamptz NOT NULL,
    PRIMARY KEY (gateway_id, event_id)
);

ALTER TABLE gateway_provider_connection_inbox ADD COLUMN IF NOT EXISTS gateway_id text;
UPDATE gateway_provider_connection_inbox SET gateway_id = 'legacy-gateway' WHERE gateway_id IS NULL;
ALTER TABLE gateway_provider_connection_inbox ALTER COLUMN gateway_id SET NOT NULL;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'gateway_provider_connection_inbox'::regclass
          AND contype = 'p'
          AND conkey = ARRAY[
              (SELECT attnum FROM pg_attribute WHERE attrelid='gateway_provider_connection_inbox'::regclass AND attname='gateway_id'),
              (SELECT attnum FROM pg_attribute WHERE attrelid='gateway_provider_connection_inbox'::regclass AND attname='event_id')
          ]::smallint[]
    ) THEN
        ALTER TABLE gateway_provider_connection_inbox DROP CONSTRAINT IF EXISTS gateway_provider_connection_inbox_pkey;
        ALTER TABLE gateway_provider_connection_inbox ADD PRIMARY KEY (gateway_id, event_id);
    END IF;
END $$;
