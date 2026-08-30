CREATE TABLE IF NOT EXISTS control_event_history (
    singleton       boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    minimum_cursor  bigint NOT NULL DEFAULT 0 CHECK (minimum_cursor >= 0),
    updated_at      timestamptz NOT NULL
);

INSERT INTO control_event_history (singleton,minimum_cursor,updated_at)
VALUES (true,0,now()) ON CONFLICT (singleton) DO NOTHING;

-- Control Events use their delivery sequence as a durable cursor. Serializing
-- inserts prevents a later sequence from committing and becoming visible
-- before an earlier in-flight sequence.
CREATE OR REPLACE FUNCTION serialize_control_outbox_delivery()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('control-outbox-delivery',0));
    RETURN NEW;
END $$;

CREATE TABLE IF NOT EXISTS control_event_region_heads (
    region         text PRIMARY KEY,
    access_cursor  bigint NOT NULL CHECK (access_cursor >= 0),
    updated_at     timestamptz NOT NULL
);

CREATE OR REPLACE FUNCTION record_control_event_region_head()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    target_region text;
BEGIN
    target_region := NEW.payload->>'home_region';
    IF NEW.aggregate_type IN ('Tenant','GatewayAPIKey') AND NEW.schema_version=2
       AND COALESCE(target_region,'')<>'' THEN
        EXECUTE format('INSERT INTO %I.control_event_region_heads (region,access_cursor,updated_at)
            VALUES ($1,$2,$3) ON CONFLICT (region) DO UPDATE SET
            access_cursor=GREATEST(control_event_region_heads.access_cursor,EXCLUDED.access_cursor),
            updated_at=GREATEST(control_event_region_heads.updated_at,EXCLUDED.updated_at)', TG_TABLE_SCHEMA)
        USING target_region,NEW.delivery_sequence,NEW.occurred_at;
    END IF;
    RETURN NEW;
END $$;

DO $$
BEGIN
    IF to_regclass('control_outbox') IS NOT NULL THEN
        EXECUTE 'DROP TRIGGER IF EXISTS control_outbox_serialize_delivery_trigger ON control_outbox';
        EXECUTE 'CREATE TRIGGER control_outbox_serialize_delivery_trigger
            BEFORE INSERT ON control_outbox
            FOR EACH ROW EXECUTE FUNCTION serialize_control_outbox_delivery()';

        INSERT INTO control_event_region_heads (region,access_cursor,updated_at)
        SELECT payload->>'home_region',max(delivery_sequence),now()
        FROM control_outbox
        WHERE aggregate_type IN ('Tenant','GatewayAPIKey') AND schema_version=2
          AND COALESCE(payload->>'home_region','')<>''
        GROUP BY payload->>'home_region'
        ON CONFLICT (region) DO UPDATE SET
            access_cursor=GREATEST(control_event_region_heads.access_cursor,EXCLUDED.access_cursor),
            updated_at=EXCLUDED.updated_at;

        EXECUTE 'DROP TRIGGER IF EXISTS control_outbox_region_head_trigger ON control_outbox';
        EXECUTE 'CREATE TRIGGER control_outbox_region_head_trigger
            AFTER INSERT ON control_outbox
            FOR EACH ROW EXECUTE FUNCTION record_control_event_region_head()';
    END IF;
END $$;

UPDATE gateway_schema_metadata SET current_version=23,updated_at=now()
WHERE component='gateway' AND current_version<23;
