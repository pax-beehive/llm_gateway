\set ON_ERROR_STOP on

-- The roles must already exist and be managed by deployment infrastructure.
-- Run this script as the schema owner after migrations, never as a runtime role.
SELECT :'tenant_admin_role' = :'gateway_role' AS roles_are_same \gset
\if :roles_are_same
    \echo 'tenant_admin_role and gateway_role must be different'
    \quit 3
\endif

SELECT EXISTS (
    SELECT 1
    FROM pg_class AS relation
    JOIN pg_roles AS owner ON owner.oid = relation.relowner
    WHERE relation.oid IN (
        'tenants'::regclass,
        'tenant_policy_revisions'::regclass,
        'api_keys'::regclass,
        'api_key_policy_revisions'::regclass,
        'control_command_idempotency'::regclass,
        'control_audit_events'::regclass,
        'control_outbox'::regclass
    ) AND owner.rolname = :'gateway_role'
) AS gateway_owns_control_tables \gset
\if :gateway_owns_control_tables
    \echo 'gateway_role owns a control table; transfer ownership to a non-runtime migration role first'
    \quit 3
\endif

REVOKE ALL PRIVILEGES ON TABLE
    tenants,
    tenant_policy_revisions,
    api_keys,
    api_key_policy_revisions,
    control_command_idempotency,
    control_audit_events,
    control_outbox
FROM PUBLIC;

REVOKE ALL PRIVILEGES ON TABLE
    tenants,
    tenant_policy_revisions,
    api_keys,
    api_key_policy_revisions,
    control_command_idempotency,
    control_audit_events,
    control_outbox
FROM :"tenant_admin_role", :"gateway_role";

GRANT SELECT, INSERT ON TABLE tenants TO :"tenant_admin_role";
GRANT UPDATE (
    display_name,
    status,
    metadata,
    suspended_at,
    closed_at,
    policy_revision,
    policy,
    revision,
    updated_at
) ON tenants TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE tenant_policy_revisions TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE api_keys TO :"tenant_admin_role";
GRANT UPDATE (
    name,
    status,
    revision,
    policy_revision,
    policy,
    metadata,
    expires_at,
    revoked_at,
    updated_at,
    predecessor_id,
    replacement_id,
    grace_expires_at
) ON api_keys TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE api_key_policy_revisions TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE control_command_idempotency TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE control_audit_events TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE control_outbox TO :"tenant_admin_role";
GRANT UPDATE (publish_attempts, published_at, last_error) ON control_outbox TO :"tenant_admin_role";

-- The temporary Gateway adapter is read-only until ADR 0004 replaces it with
-- the local Gateway Access Projection.
GRANT SELECT ON TABLE tenants, tenant_policy_revisions, api_keys, api_key_policy_revisions TO :"gateway_role";
