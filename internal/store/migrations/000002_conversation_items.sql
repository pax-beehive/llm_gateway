ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS active_response_id text;

CREATE TABLE IF NOT EXISTS conversation_items (
    tenant_id           text NOT NULL,
    conversation_id     text NOT NULL,
    position            bigint NOT NULL CHECK (position > 0),
    item_id             text NOT NULL,
    response_id         text,
    direction           text NOT NULL CHECK (direction IN ('initial', 'input', 'output', 'manual')),
    payload             jsonb NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, conversation_id, position),
    UNIQUE (tenant_id, conversation_id, item_id),
    FOREIGN KEY (tenant_id, conversation_id) REFERENCES conversations(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS conversation_items_response_idx
    ON conversation_items (tenant_id, response_id)
    WHERE response_id IS NOT NULL;
