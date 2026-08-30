ALTER TABLE control_audit_events
    ALTER COLUMN tenant_id DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS aggregate_type text,
    ADD COLUMN IF NOT EXISTS aggregate_id text;

ALTER TABLE control_outbox ALTER COLUMN tenant_id DROP NOT NULL;

CREATE TABLE IF NOT EXISTS routing_catalog_drafts (
    id                  text PRIMARY KEY,
    base_revision       bigint NOT NULL CHECK (base_revision >= 0),
    document            jsonb NOT NULL,
    status              text NOT NULL CHECK (status IN ('draft','validated')),
    revision            bigint NOT NULL CHECK (revision > 0),
    validation_report   jsonb,
    validation_hash     text,
    created_by          text NOT NULL,
    updated_by          text NOT NULL,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS routing_catalog_revisions (
    revision            bigint PRIMARY KEY CHECK (revision > 0),
    document            jsonb NOT NULL,
    validation_report   jsonb NOT NULL,
    validation_hash     text NOT NULL,
    source_revision     bigint REFERENCES routing_catalog_revisions(revision) ON DELETE RESTRICT,
    created_by          text NOT NULL,
    created_at          timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS routing_catalog_head (
    singleton           boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    revision            bigint NOT NULL REFERENCES routing_catalog_revisions(revision) ON DELETE RESTRICT,
    updated_at          timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS routing_publications (
    id                  text PRIMARY KEY,
    catalog_revision    bigint NOT NULL UNIQUE REFERENCES routing_catalog_revisions(revision) ON DELETE RESTRICT,
    status              text NOT NULL CHECK (status IN ('published','rolling_out','active','partially_applied','failed')),
    validation_hash     text NOT NULL,
    required_regions    text[] NOT NULL DEFAULT '{}',
    created_by          text NOT NULL,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS routing_rollout_receipts (
    publication_id      text NOT NULL REFERENCES routing_publications(id) ON DELETE RESTRICT,
    gateway_id          text NOT NULL,
    region              text NOT NULL,
    catalog_revision    bigint NOT NULL,
    status              text NOT NULL CHECK (status IN ('applied','rejected')),
    error_code          text,
    observed_at         timestamptz NOT NULL,
    PRIMARY KEY (publication_id, gateway_id, region)
);

CREATE INDEX IF NOT EXISTS routing_catalog_drafts_updated_idx
    ON routing_catalog_drafts (updated_at, id);
CREATE INDEX IF NOT EXISTS routing_rollout_receipts_revision_idx
    ON routing_rollout_receipts (catalog_revision, region, gateway_id);

CREATE OR REPLACE FUNCTION reject_routing_catalog_revision_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Routing Catalog revisions are append-only';
END $$;

DROP TRIGGER IF EXISTS routing_catalog_revisions_append_only_trigger ON routing_catalog_revisions;
CREATE TRIGGER routing_catalog_revisions_append_only_trigger
    BEFORE UPDATE OR DELETE ON routing_catalog_revisions
    FOR EACH ROW EXECUTE FUNCTION reject_routing_catalog_revision_mutation();
