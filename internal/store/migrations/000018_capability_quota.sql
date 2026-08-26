ALTER TABLE quota_reservations
    ADD COLUMN IF NOT EXISTS capability_operation_id text,
    ADD COLUMN IF NOT EXISTS capability text,
    ADD COLUMN IF NOT EXISTS capability_home_region text,
    ADD COLUMN IF NOT EXISTS capability_execution_epoch bigint,
    ADD COLUMN IF NOT EXISTS reserved_embedding_input_units bigint NOT NULL DEFAULT 0 CHECK (reserved_embedding_input_units >= 0),
    ADD COLUMN IF NOT EXISTS committed_embedding_input_units bigint NOT NULL DEFAULT 0 CHECK (committed_embedding_input_units >= 0),
    ADD COLUMN IF NOT EXISTS reserved_rerank_documents bigint NOT NULL DEFAULT 0 CHECK (reserved_rerank_documents >= 0),
    ADD COLUMN IF NOT EXISTS committed_rerank_documents bigint NOT NULL DEFAULT 0 CHECK (committed_rerank_documents >= 0);

UPDATE quota_reservations q
SET capability_home_region = t.home_region,
    capability_execution_epoch = t.execution_epoch
FROM tenants t
WHERE q.tenant_id = t.id AND q.kind = 'capability'
  AND (q.capability_home_region IS NULL OR q.capability_execution_epoch IS NULL);

ALTER TABLE quota_reservations DROP CONSTRAINT IF EXISTS quota_reservations_kind_check;
ALTER TABLE quota_reservations DROP CONSTRAINT IF EXISTS quota_reservations_identity_check;
ALTER TABLE quota_reservations DROP CONSTRAINT IF EXISTS quota_reservations_capability_check;

ALTER TABLE quota_reservations DROP CONSTRAINT IF EXISTS quota_reservations_capability_writer_check;

ALTER TABLE quota_reservations ADD CONSTRAINT quota_reservations_kind_check
    CHECK (kind IN ('response', 'cache_refresh', 'capability'));

ALTER TABLE quota_reservations ADD CONSTRAINT quota_reservations_capability_check
    CHECK (capability IS NULL OR capability IN ('embeddings', 'moderation', 'rerank'));

ALTER TABLE quota_reservations ADD CONSTRAINT quota_reservations_capability_writer_check CHECK (
    (kind = 'capability' AND capability_home_region IS NOT NULL AND capability_execution_epoch > 0)
    OR (kind <> 'capability' AND capability_home_region IS NULL AND capability_execution_epoch IS NULL)
);

ALTER TABLE quota_reservations ADD CONSTRAINT quota_reservations_identity_check CHECK (
    (kind = 'response' AND response_id IS NOT NULL AND cache_refresh_intent_id IS NULL
        AND capability_operation_id IS NULL AND capability IS NULL)
    OR (kind = 'cache_refresh' AND response_id IS NULL AND cache_refresh_intent_id IS NOT NULL
        AND capability_operation_id IS NULL AND capability IS NULL)
    OR (kind = 'capability' AND response_id IS NULL AND cache_refresh_intent_id IS NULL
        AND capability_operation_id IS NOT NULL AND capability IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS quota_reservations_capability_operation_unique_idx
    ON quota_reservations (tenant_id, capability_operation_id)
    WHERE capability_operation_id IS NOT NULL;
