CREATE TABLE IF NOT EXISTS control_command_idempotency (
    actor_type       text NOT NULL,
    actor_id         text NOT NULL,
    operation        text NOT NULL,
    idempotency_key  text NOT NULL,
    request_hash     bytea NOT NULL,
    result           jsonb NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_type, actor_id, operation, idempotency_key)
);

CREATE TABLE IF NOT EXISTS control_audit_events (
    event_id          text PRIMARY KEY,
    tenant_id         text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    actor_type        text NOT NULL,
    actor_id          text NOT NULL,
    acting_tenant_id  text,
    scopes            text[] NOT NULL,
    request_id        text NOT NULL,
    reason            text NOT NULL,
    action            text NOT NULL,
    aggregate_revision bigint NOT NULL CHECK (aggregate_revision > 0),
    payload           jsonb NOT NULL,
    occurred_at       timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS control_audit_tenant_time_idx
    ON control_audit_events (tenant_id, occurred_at, event_id);

CREATE OR REPLACE FUNCTION reject_control_audit_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'control audit events are append-only';
END $$;

DROP TRIGGER IF EXISTS control_audit_append_only_trigger ON control_audit_events;
CREATE TRIGGER control_audit_append_only_trigger
    BEFORE UPDATE OR DELETE ON control_audit_events
    FOR EACH ROW EXECUTE FUNCTION reject_control_audit_mutation();

CREATE TABLE IF NOT EXISTS control_outbox (
    event_id           text PRIMARY KEY,
    schema_version     integer NOT NULL CHECK (schema_version > 0),
    aggregate_type     text NOT NULL,
    aggregate_id       text NOT NULL,
    aggregate_revision bigint NOT NULL CHECK (aggregate_revision > 0),
    tenant_id          text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    event_type         text NOT NULL,
    occurred_at        timestamptz NOT NULL,
    payload            jsonb NOT NULL,
    publish_attempts   integer NOT NULL DEFAULT 0 CHECK (publish_attempts >= 0),
    published_at       timestamptz,
    last_error         text,
    UNIQUE (aggregate_type, aggregate_id, aggregate_revision, event_type)
);

ALTER TABLE control_outbox
    ADD COLUMN IF NOT EXISTS delivery_sequence bigserial;

CREATE UNIQUE INDEX IF NOT EXISTS control_outbox_delivery_sequence_idx
    ON control_outbox (delivery_sequence);

CREATE INDEX IF NOT EXISTS control_outbox_unpublished_idx
    ON control_outbox (occurred_at, event_id) WHERE published_at IS NULL;

-- Control-plane creates attribute revision 1 directly through transaction-local
-- actor settings. Legacy inserts keep the compatibility attribution.
CREATE OR REPLACE FUNCTION tenants_record_initial_policy()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO tenant_policy_revisions (
        tenant_id, revision, policy, actor_type, actor_id, change_reason
    ) VALUES (
        NEW.id,
        NEW.policy_revision,
        NEW.policy,
        COALESCE(NULLIF(current_setting('app.control_actor_type', true), ''), 'compatibility'),
        COALESCE(NULLIF(current_setting('app.control_actor_id', true), ''), 'tenant-insert-trigger'),
        COALESCE(NULLIF(current_setting('app.control_change_reason', true), ''), 'initial policy for legacy Tenant insert')
    ) ON CONFLICT (tenant_id, revision) DO NOTHING;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION reject_tenant_policy_revision_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- A Tenant-level delete may cascade its owned history. Direct history
    -- mutation remains forbidden to every application caller.
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1 THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'Tenant Policy revisions are append-only';
END $$;

DROP TRIGGER IF EXISTS tenant_policy_revisions_append_only_trigger ON tenant_policy_revisions;
CREATE TRIGGER tenant_policy_revisions_append_only_trigger
    BEFORE UPDATE OR DELETE ON tenant_policy_revisions
    FOR EACH ROW EXECUTE FUNCTION reject_tenant_policy_revision_mutation();
