CREATE TABLE IF NOT EXISTS configuration_history (
    kind                text NOT NULL,
    revision            bigint NOT NULL CHECK (revision > 0),
    payload             jsonb NOT NULL,
    created_by          text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, revision)
);

CREATE TABLE IF NOT EXISTS configuration_heads (
    kind                text PRIMARY KEY,
    revision            bigint NOT NULL CHECK (revision > 0),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (kind, revision) REFERENCES configuration_history(kind, revision)
);
