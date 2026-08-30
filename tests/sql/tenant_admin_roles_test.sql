\set ON_ERROR_STOP on

BEGIN;
CREATE ROLE llm_gateway_tenant_admin_test NOLOGIN;
CREATE ROLE llm_gateway_runtime_test NOLOGIN;
\set tenant_admin_role llm_gateway_tenant_admin_test
\set gateway_role llm_gateway_runtime_test
\ir ../../scripts/postgres/configure-tenant-admin-roles.sql

DO $$
BEGIN
    IF NOT has_table_privilege('llm_gateway_tenant_admin_test', 'tenants', 'INSERT')
       OR NOT has_column_privilege('llm_gateway_tenant_admin_test', 'tenants', 'display_name', 'UPDATE')
       OR has_column_privilege('llm_gateway_tenant_admin_test', 'tenants', 'home_region', 'UPDATE')
       OR NOT has_table_privilege('llm_gateway_tenant_admin_test', 'control_audit_events', 'INSERT')
       OR has_table_privilege('llm_gateway_tenant_admin_test', 'control_audit_events', 'DELETE')
       OR NOT has_table_privilege('llm_gateway_tenant_admin_test', 'api_keys', 'INSERT')
       OR NOT has_column_privilege('llm_gateway_tenant_admin_test', 'api_keys', 'status', 'UPDATE')
       OR has_column_privilege('llm_gateway_tenant_admin_test', 'api_keys', 'secret_digest', 'UPDATE')
	   OR NOT has_table_privilege('llm_gateway_tenant_admin_test', 'provider_connections', 'INSERT')
	   OR NOT has_column_privilege('llm_gateway_tenant_admin_test', 'provider_connections', 'administrative_status', 'UPDATE')
	   OR NOT has_column_privilege('llm_gateway_tenant_admin_test', 'provider_operations', 'status', 'UPDATE')
	   OR has_column_privilege('llm_gateway_tenant_admin_test', 'provider_operations', 'actor_id', 'UPDATE')
	   OR has_column_privilege('llm_gateway_tenant_admin_test', 'provider_connection_credential_versions', 'secret_ref', 'UPDATE')
	   OR has_table_privilege('llm_gateway_runtime_test', 'provider_connections', 'SELECT')
	   OR NOT has_table_privilege('llm_gateway_runtime_test', 'gateway_provider_connection_resolutions', 'SELECT')
	   OR NOT has_table_privilege('llm_gateway_tenant_admin_test', 'gateway_routing_catalog_inbox', 'SELECT')
	   OR NOT has_table_privilege('llm_gateway_tenant_admin_test', 'routing_rollout_receipts', 'INSERT')
	   OR NOT has_table_privilege('llm_gateway_runtime_test', 'gateway_routing_catalog_inbox', 'INSERT')
	   OR has_table_privilege('llm_gateway_runtime_test', 'gateway_routing_catalog_inbox', 'UPDATE')
	   OR has_table_privilege('llm_gateway_runtime_test', 'gateway_routing_catalog_inbox', 'DELETE')
	   OR has_table_privilege('llm_gateway_runtime_test', 'gateway_routing_catalog_history', 'DELETE')
	   OR has_table_privilege('llm_gateway_runtime_test', 'routing_rollout_receipts', 'INSERT')
	   OR has_column_privilege('llm_gateway_runtime_test', 'routing_publications', 'status', 'UPDATE')
       OR has_table_privilege('llm_gateway_runtime_test', 'tenants', 'SELECT')
       OR has_table_privilege('llm_gateway_runtime_test', 'api_keys', 'SELECT')
       OR NOT has_table_privilege('llm_gateway_runtime_test', 'control_outbox', 'SELECT')
       OR NOT has_table_privilege('llm_gateway_runtime_test', 'gateway_access_projection', 'SELECT')
       OR NOT has_table_privilege('llm_gateway_runtime_test', 'gateway_access_projection', 'INSERT')
       OR NOT has_table_privilege('llm_gateway_runtime_test', 'gateway_access_response_slots', 'DELETE')
       OR has_table_privilege('llm_gateway_runtime_test', 'gateway_access_projection', 'TRUNCATE')
       OR has_table_privilege('llm_gateway_runtime_test', 'tenants', 'INSERT')
       OR has_table_privilege('llm_gateway_runtime_test', 'tenant_policy_revisions', 'UPDATE') THEN
        RAISE EXCEPTION 'Tenant Administration role isolation assertion failed';
    END IF;
END $$;

ROLLBACK;
