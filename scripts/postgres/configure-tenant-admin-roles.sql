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
        'control_outbox'::regclass,
		'control_event_history'::regclass,
		'control_event_region_heads'::regclass,
		'provider_connections'::regclass,
		'provider_connection_credential_versions'::regclass,
		'provider_operations'::regclass,
		'provider_connection_health'::regclass,
		'provider_model_observations'::regclass,
		'gateway_provider_connection_resolutions'::regclass,
		'routing_catalog_drafts'::regclass,
		'routing_catalog_revisions'::regclass,
		'routing_catalog_head'::regclass,
		'routing_publications'::regclass,
		'routing_rollout_receipts'::regclass,
		'gateway_routing_catalog_history'::regclass,
		'gateway_routing_catalog_head'::regclass,
		'gateway_routing_catalog_inbox'::regclass,
		'gateway_provider_connection_inbox'::regclass,
		'gateway_control_event_offsets'::regclass,
		'gateway_provider_connection_projection'::regclass,
		'gateway_provider_connection_projection_inbox'::regclass,
		'gateway_provider_connection_projection_gaps'::regclass,
		'operations_schema_metadata'::regclass,
		'operations_gateway_heartbeats'::regclass,
		'operations_access_rollout_receipts'::regclass,
		'gateway_schema_metadata'::regclass,
        'gateway_access_projection'::regclass,
        'gateway_access_inbox'::regclass,
        'gateway_access_heads'::regclass,
        'gateway_access_gaps'::regclass,
        'gateway_access_rollout_receipts'::regclass,
        'gateway_access_response_slots'::regclass
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
    control_outbox,
	control_event_history,
	control_event_region_heads,
	provider_connections,
	provider_connection_credential_versions,
	provider_operations,
	provider_connection_health,
	provider_model_observations,
	gateway_provider_connection_resolutions,
	routing_catalog_drafts,
	routing_catalog_revisions,
	routing_catalog_head,
	routing_publications,
	routing_rollout_receipts,
	gateway_routing_catalog_history,
	gateway_routing_catalog_head,
	gateway_routing_catalog_inbox,
	gateway_provider_connection_inbox,
	gateway_control_event_offsets,
	gateway_provider_connection_projection,
	gateway_provider_connection_projection_inbox,
	gateway_provider_connection_projection_gaps,
	operations_schema_metadata,
	operations_gateway_heartbeats,
	operations_access_rollout_receipts,
	gateway_schema_metadata,
    gateway_access_projection,
    gateway_access_inbox,
    gateway_access_heads,
    gateway_access_gaps,
    gateway_access_rollout_receipts,
    gateway_access_response_slots
FROM PUBLIC;

REVOKE ALL PRIVILEGES ON TABLE
    tenants,
    tenant_policy_revisions,
    api_keys,
    api_key_policy_revisions,
    control_command_idempotency,
    control_audit_events,
    control_outbox,
	control_event_history,
	control_event_region_heads,
	provider_connections,
	provider_connection_credential_versions,
	provider_operations,
	provider_connection_health,
	provider_model_observations,
	gateway_provider_connection_resolutions,
	routing_catalog_drafts,
	routing_catalog_revisions,
	routing_catalog_head,
	routing_publications,
	routing_rollout_receipts,
	gateway_routing_catalog_history,
	gateway_routing_catalog_head,
	gateway_routing_catalog_inbox,
	gateway_provider_connection_inbox,
	gateway_control_event_offsets,
	gateway_provider_connection_projection,
	gateway_provider_connection_projection_inbox,
	gateway_provider_connection_projection_gaps,
	operations_schema_metadata,
	operations_gateway_heartbeats,
	operations_access_rollout_receipts,
	gateway_schema_metadata,
    gateway_access_projection,
    gateway_access_inbox,
    gateway_access_heads,
    gateway_access_gaps,
    gateway_access_rollout_receipts,
    gateway_access_response_slots
FROM :"tenant_admin_role", :"gateway_role";

REVOKE ALL PRIVILEGES ON SEQUENCE control_outbox_delivery_sequence_seq
FROM PUBLIC, :"tenant_admin_role", :"gateway_role";

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
GRANT SELECT ON TABLE control_event_history TO :"tenant_admin_role";
GRANT SELECT ON TABLE control_event_region_heads TO :"tenant_admin_role";
GRANT USAGE, SELECT ON SEQUENCE control_outbox_delivery_sequence_seq TO :"tenant_admin_role";
GRANT UPDATE (publish_attempts, published_at, last_error) ON control_outbox TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE provider_connections TO :"tenant_admin_role";
GRANT UPDATE (
	display_name, base_url, region, credential_scope, secret_ref, secret_external_version,
	credential_version, administrative_status, capability_declaration, revision, updated_at
) ON provider_connections TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE provider_connection_credential_versions TO :"tenant_admin_role";
GRANT UPDATE (status, retired_at) ON provider_connection_credential_versions TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE provider_operations TO :"tenant_admin_role";
GRANT UPDATE (status, result, error_code, error_message, started_at, completed_at, attempts, lease_expires_at)
ON provider_operations TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE provider_connection_health TO :"tenant_admin_role";
GRANT UPDATE (observed_status, error_code, operation_id, latency_milliseconds, observed_at)
ON provider_connection_health TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE provider_model_observations TO :"tenant_admin_role";
GRANT SELECT, INSERT, UPDATE ON TABLE routing_catalog_drafts TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE routing_catalog_revisions TO :"tenant_admin_role";
GRANT SELECT, INSERT, UPDATE ON TABLE routing_catalog_head TO :"tenant_admin_role";
GRANT SELECT, INSERT, UPDATE ON TABLE routing_publications TO :"tenant_admin_role";
GRANT SELECT, INSERT, UPDATE ON TABLE routing_rollout_receipts TO :"tenant_admin_role";
GRANT SELECT ON TABLE gateway_routing_catalog_inbox TO :"tenant_admin_role";
GRANT SELECT ON TABLE operations_schema_metadata TO :"tenant_admin_role";
GRANT SELECT, INSERT, UPDATE ON TABLE operations_gateway_heartbeats TO :"tenant_admin_role";
GRANT SELECT, INSERT ON TABLE operations_access_rollout_receipts TO :"tenant_admin_role";
REVOKE ALL PRIVILEGES ON TABLE tenant_quota_counters,api_key_quota_counters,quota_reservations
FROM PUBLIC, :"tenant_admin_role";
GRANT SELECT ON TABLE tenant_quota_counters,api_key_quota_counters,quota_reservations TO :"tenant_admin_role";

GRANT SELECT ON TABLE gateway_schema_metadata TO :"gateway_role";
GRANT SELECT, INSERT ON TABLE gateway_routing_catalog_history TO :"gateway_role";
GRANT SELECT, INSERT, UPDATE ON TABLE gateway_routing_catalog_head TO :"gateway_role";
GRANT SELECT, INSERT ON TABLE gateway_routing_catalog_inbox TO :"gateway_role";
GRANT SELECT, INSERT ON TABLE gateway_provider_connection_inbox TO :"gateway_role";
GRANT SELECT, INSERT, UPDATE ON TABLE gateway_control_event_offsets TO :"gateway_role";
GRANT SELECT, INSERT, UPDATE ON TABLE gateway_provider_connection_projection TO :"gateway_role";
GRANT SELECT, INSERT ON TABLE gateway_provider_connection_projection_inbox TO :"gateway_role";
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE gateway_provider_connection_projection_gaps TO :"gateway_role";
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE
    gateway_access_projection,
    gateway_access_inbox,
    gateway_access_heads,
    gateway_access_gaps,
    gateway_access_response_slots
TO :"gateway_role";
GRANT SELECT, INSERT ON TABLE gateway_access_rollout_receipts TO :"gateway_role";
GRANT UPDATE (reported_at) ON TABLE gateway_access_rollout_receipts TO :"gateway_role";
