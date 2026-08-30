CREATE TABLE IF NOT EXISTS gateway_schema_metadata (
    component       text PRIMARY KEY,
    current_version integer NOT NULL CHECK (current_version > 0),
    updated_at      timestamptz NOT NULL
);

INSERT INTO gateway_schema_metadata (component,current_version,updated_at)
VALUES ('gateway',21,now())
ON CONFLICT (component) DO UPDATE SET
    current_version=GREATEST(gateway_schema_metadata.current_version,EXCLUDED.current_version),
    updated_at=EXCLUDED.updated_at;
