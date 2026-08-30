ALTER TABLE capability_usage_ledger
    ADD COLUMN IF NOT EXISTS public_model text NOT NULL DEFAULT '';

UPDATE capability_usage_ledger
SET public_model = model
WHERE public_model = '';
