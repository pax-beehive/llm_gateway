ALTER TABLE quota_reservations
    ADD COLUMN IF NOT EXISTS response_attempt_id text;

UPDATE quota_reservations
SET response_attempt_id = response_id
WHERE kind = 'response' AND response_attempt_id IS NULL;

ALTER TABLE quota_reservations
    DROP CONSTRAINT IF EXISTS quota_reservations_tenant_id_response_id_key;

DROP INDEX IF EXISTS quota_reservations_response_attempt_unique_idx;
CREATE UNIQUE INDEX quota_reservations_response_attempt_unique_idx
    ON quota_reservations (tenant_id, response_id, response_attempt_id)
    WHERE response_id IS NOT NULL;

ALTER TABLE quota_reservations
    DROP CONSTRAINT IF EXISTS quota_reservations_response_attempt_check;

ALTER TABLE quota_reservations
    ADD CONSTRAINT quota_reservations_response_attempt_check CHECK (
        (kind = 'response' AND response_attempt_id IS NOT NULL)
        OR (kind <> 'response' AND response_attempt_id IS NULL)
    );
