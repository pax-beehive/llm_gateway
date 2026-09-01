CREATE TABLE IF NOT EXISTS metering_quota_denials (
    event_id                  text PRIMARY KEY REFERENCES metering_inbox(event_id),
    tenant_id                text NOT NULL,
    api_key_id               text,
    response_id              text,
    attempt_id               text,
    operation_id             text,
    capability               text,
    public_model             text,
    route_id                 text,
    region                   text,
    denial_scope             text NOT NULL,
    dimension                text NOT NULL,
    currency                 text,
    tenant_policy_revision   bigint,
    api_key_policy_revision  bigint,
    occurred_at              timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS metering_quota_denials_tenant_time_idx
    ON metering_quota_denials (tenant_id, occurred_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS metering_quota_denials_key_time_idx
    ON metering_quota_denials (tenant_id, api_key_id, occurred_at DESC, event_id DESC)
    WHERE api_key_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS metering_quota_denials_route_time_idx
    ON metering_quota_denials (route_id, occurred_at DESC, event_id DESC)
    WHERE route_id IS NOT NULL;
