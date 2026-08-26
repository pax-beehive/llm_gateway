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
       OR NOT has_table_privilege('llm_gateway_runtime_test', 'tenants', 'SELECT')
       OR has_table_privilege('llm_gateway_runtime_test', 'tenants', 'INSERT')
       OR has_table_privilege('llm_gateway_runtime_test', 'tenant_policy_revisions', 'UPDATE') THEN
        RAISE EXCEPTION 'Tenant Administration role isolation assertion failed';
    END IF;
END $$;

ROLLBACK;
