CREATE TABLE IF NOT EXISTS tenant_response_slots (
    tenant_id  text NOT NULL REFERENCES tenants(id),
    lease_id   text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, lease_id)
);

CREATE INDEX IF NOT EXISTS tenant_response_slots_expiry_idx
    ON tenant_response_slots (tenant_id, expires_at);
