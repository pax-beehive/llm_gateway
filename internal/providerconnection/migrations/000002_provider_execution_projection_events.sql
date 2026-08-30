-- Schema 3 is the secret-free execution projection contract. Backfill one
-- current-state event so a Gateway can bootstrap without replaying legacy
-- schema-2 Provider Connection events whose payload lacked execution metadata.
INSERT INTO control_outbox (
    event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,
    tenant_id,event_type,occurred_at,payload
)
SELECT 'cevt_projection_' || md5(c.id || ':' || c.revision::text),3,'ProviderConnection',c.id,c.revision,
       NULL,'ProviderConnectionExecutionProjected',c.updated_at,
       jsonb_build_object(
           'connection_id',c.id,
           'provider',c.provider,
           'base_url',c.base_url,
           'region',c.region,
           'administrative_status',c.administrative_status,
           'credential_scope',c.credential_scope,
           'capability_declaration',c.capability_declaration,
           'credential_version',c.credential_version,
           'revision',c.revision
       )
FROM provider_connections c
ON CONFLICT (aggregate_type,aggregate_id,aggregate_revision,event_type) DO NOTHING;
