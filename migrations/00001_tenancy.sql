-- Tenancy, identity mirror, API keys and the audit log (F01, ADR-0008, ADR-0012).
--
-- Every tenant-owned table has tenant_id and a row-level security policy on
-- app.tenant_id. monitor_app is subject to it; monitor_system bypasses it.

-- +goose Up

-- One row: the identity of this engine installation, set by
-- `monitor admin bootstrap`. Its presence means the install is bootstrapped.
CREATE TABLE installation (
    singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id         uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tenants (
    id                         uuid PRIMARY KEY,
    name                       text        NOT NULL,
    status                     text        NOT NULL DEFAULT 'active'
                               CHECK (status IN ('active', 'suspended', 'deleted')),
    is_platform                boolean     NOT NULL DEFAULT false,
    config_source              text        NOT NULL DEFAULT 'api' CHECK (config_source IN ('api', 'file')),
    max_devices                integer     NOT NULL CHECK (max_devices >= 0),
    max_checks                 integer     NOT NULL CHECK (max_checks >= 0),
    min_check_interval_seconds integer     NOT NULL CHECK (min_check_interval_seconds >= 1),
    api_rate_per_minute        integer     NOT NULL CHECK (api_rate_per_minute >= 0),
    allowed_target_networks    cidr[]      NOT NULL DEFAULT '{}',
    metric_classes             text[]      NOT NULL DEFAULT '{standard}',
    max_api_key_lifetime_days  integer     CHECK (max_api_key_lifetime_days >= 1),
    external_source            text,
    external_type              text,
    external_id                text,
    provisioned                text        NOT NULL DEFAULT 'api' CHECK (provisioned IN ('api', 'jit', 'bootstrap')),
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    deleted_at                 timestamptz,
    CHECK ((external_source IS NULL) = (external_id IS NULL))
);

CREATE UNIQUE INDEX tenants_external ON tenants (external_source, external_id)
    WHERE external_id IS NOT NULL;
CREATE UNIQUE INDEX tenants_one_platform ON tenants (is_platform) WHERE is_platform;

-- Users are global: one row per PlusClouds user. No personal data (F01):
-- a migration test fails if an email or phone column appears.
CREATE TABLE users (
    id              uuid PRIMARY KEY,
    display_name    text,
    status          text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    external_source text,
    external_type   text,
    external_id     text,
    provisioned     text        NOT NULL DEFAULT 'api' CHECK (provisioned IN ('api', 'jit')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((external_source IS NULL) = (external_id IS NULL))
);

CREATE UNIQUE INDEX users_external ON users (external_source, external_id)
    WHERE external_id IS NOT NULL;

CREATE TABLE tenant_members (
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text        NOT NULL CHECK (role IN ('read-only', 'operator', 'admin')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

CREATE INDEX tenant_members_user ON tenant_members (user_id);

-- Keys are stored as SHA-256 of the full key; the prefix finds the row (ADR-0007).
-- Platform keys belong to no tenant.
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY,
    tenant_id    uuid        REFERENCES tenants (id) ON DELETE CASCADE,
    kind         text        NOT NULL CHECK (kind IN ('tenant', 'platform')),
    name         text        NOT NULL,
    prefix       text        NOT NULL UNIQUE CHECK (length(prefix) = 8),
    hash         bytea       NOT NULL CHECK (length(hash) = 32),
    role         text        NOT NULL CHECK (role IN ('read-only', 'operator', 'admin', 'platform')),
    expires_at   timestamptz,
    ip_allowlist cidr[]      NOT NULL DEFAULT '{}',
    last_used_at timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'platform') = (tenant_id IS NULL)),
    CHECK ((kind = 'platform') = (role = 'platform'))
);

CREATE INDEX api_keys_tenant ON api_keys (tenant_id);

-- Audit log (F01). Each event carries a per-tenant sequence number and a
-- hash chain: hash = sha256(prev_hash and the event's fields), so changes
-- made with superuser rights are detectable by audit_verify().
CREATE TABLE audit_events (
    id                uuid PRIMARY KEY,
    tenant_id         uuid        NOT NULL REFERENCES tenants (id),
    seq               bigint      NOT NULL,
    at                timestamptz NOT NULL DEFAULT now(),
    actor_kind        text        NOT NULL
                      CHECK (actor_kind IN ('user', 'api_key', 'platform', 'admin-cli', 'file', 'system')),
    actor_user_id     uuid,
    actor_external_id text,
    api_key_id        uuid,
    actor_detail      text,
    action            text        NOT NULL,
    object_type       text        NOT NULL,
    object_id         text,
    before            jsonb,
    after             jsonb,
    request_id        text,
    source_ip         inet,
    prev_hash         bytea,
    hash              bytea       NOT NULL,
    UNIQUE (tenant_id, seq)
);

CREATE INDEX audit_events_object ON audit_events (tenant_id, object_type, object_id);
CREATE INDEX audit_events_at ON audit_events (tenant_id, at);

-- The fields covered by the hash, in a fixed text form that does not depend
-- on session settings (timestamps as epoch microseconds).
-- +goose StatementBegin
CREATE FUNCTION audit_hash(e audit_events) RETURNS bytea
LANGUAGE sql STABLE
SET search_path = pg_catalog
AS $$
    SELECT sha256(convert_to(concat_ws(E'\x1f',
        encode(coalesce(e.prev_hash, ''::bytea), 'hex'),
        e.id::text, e.tenant_id::text, e.seq::text,
        (extract(epoch FROM e.at) * 1000000)::bigint::text,
        e.actor_kind, e.actor_user_id::text, e.actor_external_id, e.api_key_id::text, e.actor_detail,
        e.action, e.object_type, e.object_id,
        e.before::text, e.after::text,
        e.request_id, e.source_ip::text
    ), 'UTF8'))
$$;
-- +goose StatementEnd

-- Assigns seq, prev_hash and hash. Events of one tenant are serialized by a
-- transaction-level advisory lock, so the chain has no forks.
-- +goose StatementBegin
CREATE FUNCTION audit_chain() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    last_seq  bigint;
    last_hash bytea;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('audit:' || NEW.tenant_id::text, 0));
    SELECT seq, hash INTO last_seq, last_hash
      FROM audit_events WHERE tenant_id = NEW.tenant_id
     ORDER BY seq DESC LIMIT 1;
    NEW.seq       := coalesce(last_seq, 0) + 1;
    NEW.prev_hash := last_hash;
    NEW.hash      := audit_hash(NEW);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_chain BEFORE INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_chain();

-- Returns the first broken event of a tenant's chain, or no row when the
-- chain is intact. Used by `monitor admin verify-audit`.
-- +goose StatementBegin
CREATE FUNCTION audit_verify(p_tenant uuid)
RETURNS TABLE (seq bigint, problem text)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, public
AS $$
DECLARE
    e         audit_events;
    want_seq  bigint := 1;
    prev      bytea;
BEGIN
    FOR e IN SELECT * FROM audit_events a WHERE a.tenant_id = p_tenant ORDER BY a.seq LOOP
        IF e.seq <> want_seq THEN
            seq := e.seq; problem := format('expected seq %s', want_seq); RETURN NEXT; RETURN;
        END IF;
        IF e.prev_hash IS DISTINCT FROM prev THEN
            seq := e.seq; problem := 'prev_hash does not match the previous event'; RETURN NEXT; RETURN;
        END IF;
        IF e.hash IS DISTINCT FROM audit_hash(e) THEN
            seq := e.seq; problem := 'hash does not match the event'; RETURN NEXT; RETURN;
        END IF;
        prev := e.hash;
        want_seq := want_seq + 1;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- API key lookup before the tenant is known (ADR-0008). Returns only what is
-- needed to verify the presented key.
-- +goose StatementBegin
CREATE FUNCTION auth_lookup_key(p_prefix text)
RETURNS TABLE (id uuid, tenant_id uuid, kind text, hash bytea, role text,
               expires_at timestamptz, ip_allowlist cidr[], revoked_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT k.id, k.tenant_id, k.kind, k.hash, k.role, k.expires_at, k.ip_allowlist, k.revoked_at
      FROM api_keys k WHERE k.prefix = p_prefix
$$;
-- +goose StatementEnd

-- Row-level security.
ALTER TABLE tenants        ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys       ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events   ENABLE ROW LEVEL SECURITY;
ALTER TABLE users          ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenants
    USING (id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON tenant_members
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON api_keys
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON audit_events
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- A user is visible to a tenant when it is a member of it.
CREATE POLICY tenant_members_only ON users
    USING (EXISTS (SELECT 1 FROM tenant_members m
                    WHERE m.user_id = users.id
                      AND m.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));

-- Privileges. The audit log is append-only for both application roles.
GRANT SELECT ON installation TO monitor_app, monitor_system;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenants, users, tenant_members, api_keys TO monitor_system;
GRANT SELECT ON tenants, users, tenant_members TO monitor_app;
GRANT SELECT, INSERT, UPDATE ON api_keys TO monitor_app;
GRANT INSERT ON installation TO monitor_system;
GRANT SELECT, INSERT ON audit_events TO monitor_app, monitor_system;
REVOKE ALL ON FUNCTION auth_lookup_key(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_lookup_key(text) TO monitor_app, monitor_system;
REVOKE ALL ON FUNCTION audit_verify(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION audit_verify(uuid) TO monitor_system;

-- +goose Down
DROP FUNCTION audit_verify(uuid);
DROP FUNCTION auth_lookup_key(text);
DROP FUNCTION audit_hash(audit_events);
DROP TABLE audit_events;
DROP FUNCTION audit_chain();
DROP TABLE api_keys;
DROP POLICY tenant_members_only ON users;
DROP TABLE tenant_members;
DROP TABLE users;
DROP TABLE tenants;
DROP TABLE installation;
