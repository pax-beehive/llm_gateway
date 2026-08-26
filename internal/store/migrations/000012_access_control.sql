ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS slug text,
    ADD COLUMN IF NOT EXISTS display_name text,
    ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    ADD COLUMN IF NOT EXISTS metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS suspended_at timestamptz,
    ADD COLUMN IF NOT EXISTS closed_at timestamptz;

UPDATE tenants SET slug = id WHERE slug IS NULL;
UPDATE tenants SET display_name = id WHERE display_name IS NULL;

ALTER TABLE tenants ALTER COLUMN slug SET NOT NULL;
ALTER TABLE tenants ALTER COLUMN display_name SET NOT NULL;

CREATE OR REPLACE FUNCTION tenants_fill_access_defaults()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.slug IS NULL OR NEW.slug = '' THEN
        NEW.slug := NEW.id;
    END IF;
    IF NEW.display_name IS NULL OR NEW.display_name = '' THEN
        NEW.display_name := NEW.id;
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS tenants_fill_access_defaults_trigger ON tenants;
CREATE TRIGGER tenants_fill_access_defaults_trigger
    BEFORE INSERT OR UPDATE OF id, slug, display_name ON tenants
    FOR EACH ROW EXECUTE FUNCTION tenants_fill_access_defaults();

CREATE UNIQUE INDEX IF NOT EXISTS tenants_slug_unique_idx ON tenants (slug);

DO $$
BEGIN
    ALTER TABLE tenants ADD CONSTRAINT tenants_status_check
        CHECK (status IN ('active', 'suspended', 'closed'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE tenants ADD CONSTRAINT tenants_lifecycle_check CHECK (
        (status = 'active' AND suspended_at IS NULL AND closed_at IS NULL)
        OR (status = 'suspended' AND suspended_at IS NOT NULL AND closed_at IS NULL)
        OR (status = 'closed' AND closed_at IS NOT NULL)
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS tenant_policy_revisions (
    tenant_id      text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    revision       bigint NOT NULL CHECK (revision > 0),
    policy         jsonb NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    change_reason  text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, revision)
);

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

DROP TRIGGER IF EXISTS tenants_record_initial_policy_trigger ON tenants;
CREATE TRIGGER tenants_record_initial_policy_trigger
    AFTER INSERT ON tenants
    FOR EACH ROW EXECUTE FUNCTION tenants_record_initial_policy();

INSERT INTO tenant_policy_revisions (tenant_id, revision, policy, actor_type, actor_id, change_reason)
SELECT id, policy_revision, policy, 'migration', '000012', 'backfill current Tenant policy'
FROM tenants
ON CONFLICT (tenant_id, revision) DO NOTHING;

DO $$
BEGIN
    ALTER TABLE tenants ADD CONSTRAINT tenants_current_policy_fk
        FOREIGN KEY (id, policy_revision)
        REFERENCES tenant_policy_revisions(tenant_id, revision)
        DEFERRABLE INITIALLY DEFERRED;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS api_keys (
    id               text PRIMARY KEY,
    tenant_id        text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name             text NOT NULL,
    key_prefix       text NOT NULL,
    secret_digest    bytea NOT NULL UNIQUE,
    digest_version   smallint NOT NULL DEFAULT 1 CHECK (digest_version > 0),
    status           text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'revoked')),
    revision         bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    policy_revision  bigint NOT NULL DEFAULT 1 CHECK (policy_revision > 0),
    policy           jsonb NOT NULL DEFAULT '{}'::jsonb,
    metadata         jsonb NOT NULL DEFAULT '{}'::jsonb,
    expires_at       timestamptz,
    revoked_at       timestamptz,
    last_used_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    CHECK (
        (status = 'active' AND revoked_at IS NULL)
        OR (status = 'revoked' AND revoked_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS api_keys_tenant_status_idx ON api_keys (tenant_id, status);

CREATE TABLE IF NOT EXISTS api_key_policy_revisions (
    tenant_id      text NOT NULL,
    api_key_id     text NOT NULL,
    revision       bigint NOT NULL CHECK (revision > 0),
    policy         jsonb NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    change_reason  text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, api_key_id, revision),
    FOREIGN KEY (tenant_id, api_key_id)
        REFERENCES api_keys(tenant_id, id) ON DELETE CASCADE
);

DO $$
BEGIN
    ALTER TABLE api_keys ADD CONSTRAINT api_keys_current_policy_fk
        FOREIGN KEY (tenant_id, id, policy_revision)
        REFERENCES api_key_policy_revisions(tenant_id, api_key_id, revision)
        DEFERRABLE INITIALLY DEFERRED;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
