-- v0.10.0: the embedded MQTT broker (F08, F12, ADR-0013). Credentials decide
-- the tenant; a device is known by the key in its topic (a FixLean sensor's
-- MAC); connections feed an mqtt.connection check per device.

-- +goose Up
CREATE TABLE mqtt_credentials (
    id            uuid        PRIMARY KEY,
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name          text        NOT NULL,
    username      text        NOT NULL UNIQUE,  -- global: the username finds the tenant
    password_hash text        NOT NULL,         -- Argon2id, PHC string
    kind          text        NOT NULL CHECK (kind IN ('device', 'shared')),
    device_key    text,                         -- device kind: the only key it may publish under
    profile       text        NOT NULL CHECK (profile IN ('fixlean-esp', 'json')),
    allow_plain   boolean     NOT NULL DEFAULT false,  -- may use the plain (legacy) listener
    auto_register boolean     NOT NULL DEFAULT true,
    enabled       boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    UNIQUE (tenant_id, name),
    CHECK ((kind = 'device') = (device_key IS NOT NULL))
);
ALTER TABLE mqtt_credentials ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mqtt_credentials
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON mqtt_credentials TO monitor_app;
GRANT SELECT, UPDATE ON mqtt_credentials TO monitor_system;

-- The device behind a topic key, with its data and connection checks.
CREATE TABLE mqtt_devices (
    tenant_id           uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    device_key          text        NOT NULL,
    device_id           uuid        NOT NULL UNIQUE REFERENCES devices (id) ON DELETE CASCADE,
    data_check_id       uuid        NOT NULL REFERENCES checks (id) ON DELETE CASCADE,
    connection_check_id uuid        REFERENCES checks (id) ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, device_key)
);
ALTER TABLE mqtt_devices ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mqtt_devices
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, DELETE ON mqtt_devices TO monitor_app;  -- bind and unbind through the API
GRANT SELECT, INSERT, DELETE ON mqtt_devices TO monitor_system;

-- Which node holds a device's connection (ADR-0013). A late disconnect
-- from an old session does not match the row and is ignored.
CREATE TABLE mqtt_sessions (
    device_id    uuid        PRIMARY KEY REFERENCES devices (id) ON DELETE CASCADE,
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    node_id      text        NOT NULL,
    client_id    text        NOT NULL,
    remote       text        NOT NULL,
    keepalive    integer     NOT NULL,
    connected_at timestamptz NOT NULL
);
ALTER TABLE mqtt_sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mqtt_sessions
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, DELETE ON mqtt_sessions TO monitor_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON mqtt_sessions TO monitor_system;

-- Keys whose messages were dropped: auto-registration off, or the tenant
-- at its device limit.
CREATE TABLE mqtt_unregistered (
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    device_key    text        NOT NULL,
    credential_id uuid,
    reason        text        NOT NULL,
    messages      bigint      NOT NULL DEFAULT 1,
    first_seen    timestamptz NOT NULL DEFAULT now(),
    last_seen     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, device_key)
);
ALTER TABLE mqtt_unregistered ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mqtt_unregistered
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, DELETE ON mqtt_unregistered TO monitor_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON mqtt_unregistered TO monitor_system;

-- MQTT data checks use the last-seen rule but have no HTTP token.
ALTER TABLE push_sources ALTER COLUMN token_hash DROP NOT NULL, ALTER COLUMN token_prefix DROP NOT NULL,
    ALTER COLUMN token_created_at DROP NOT NULL;
GRANT INSERT ON push_sources TO monitor_system;

-- +goose Down
DELETE FROM push_sources WHERE token_hash IS NULL;
ALTER TABLE push_sources ALTER COLUMN token_hash SET NOT NULL, ALTER COLUMN token_prefix SET NOT NULL,
    ALTER COLUMN token_created_at SET NOT NULL;
DROP TABLE mqtt_unregistered;
DROP TABLE mqtt_sessions;
DROP TABLE mqtt_devices;
DROP TABLE mqtt_credentials;
