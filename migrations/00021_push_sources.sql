-- v0.9.0: HTTP push checks (F08). Each push check has its own ingest
-- token and the time of its last message, which drives the last-seen rule.

-- +goose Up
CREATE TABLE push_sources (
    check_id          uuid        PRIMARY KEY,
    tenant_id         uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    token_hash        bytea       NOT NULL,  -- SHA-256 of the full token; the token is shown once
    token_prefix      text        NOT NULL,  -- first characters, to tell tokens apart
    token_created_at  timestamptz NOT NULL DEFAULT now(),
    last_push_at      timestamptz,           -- newest message time accepted
    stale_reported_at timestamptz,           -- last "no data" result sent to the engine
    created_at        timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
);
ALTER TABLE push_sources ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON push_sources
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON push_sources TO monitor_app;
GRANT SELECT, UPDATE ON push_sources TO monitor_system;

-- +goose Down
DROP TABLE push_sources;
