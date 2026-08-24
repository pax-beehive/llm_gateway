CREATE TABLE IF NOT EXISTS cache_refresh_session_budgets (
    tenant_id           text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    budget_revision     text NOT NULL,
    session_identity    text NOT NULL,
    refresh_intent_id   text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, budget_revision, session_identity)
);
