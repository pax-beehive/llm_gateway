CREATE OR REPLACE VIEW gateway_tenant_fences
WITH (security_barrier = true) AS
SELECT id AS tenant_id, home_region, execution_epoch
FROM tenants;

CREATE OR REPLACE FUNCTION gateway_lock_tenant_fence(requested_tenant_id text)
RETURNS TABLE(home_region text, execution_epoch bigint)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT tenant.home_region, tenant.execution_epoch
    FROM public.tenants AS tenant
    WHERE tenant.id = requested_tenant_id
    FOR SHARE
$$;

REVOKE ALL ON FUNCTION gateway_lock_tenant_fence(text) FROM PUBLIC;

UPDATE gateway_schema_metadata SET current_version=25,updated_at=now()
WHERE component='gateway' AND current_version<25;
