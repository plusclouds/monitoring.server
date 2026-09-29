-- Narrow cross-tenant functions for the api role (ADR-0008).
--
-- The api role is subject to row-level security. These SECURITY DEFINER
-- functions are the only things it can do before a tenant is known, or
-- across tenants for the platform key. Everything else runs inside a
-- transaction with app.tenant_id set.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION platform_tenant_id() RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public
AS $$ SELECT id FROM tenants WHERE is_platform $$;

CREATE FUNCTION tenant_by_id(p_id uuid) RETURNS SETOF tenants
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public
AS $$ SELECT * FROM tenants WHERE id = p_id $$;

CREATE FUNCTION tenant_by_external(p_source text, p_id text) RETURNS SETOF tenants
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public
AS $$ SELECT * FROM tenants WHERE external_source = p_source AND external_id = p_id $$;

-- Reconciliation listing for the platform key, ordered by id for cursors.
CREATE FUNCTION list_tenants(p_source text, p_id text, p_after uuid, p_limit integer) RETURNS SETOF tenants
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
    SELECT * FROM tenants
     WHERE NOT is_platform
       AND (p_source IS NULL OR external_source = p_source)
       AND (p_id IS NULL OR external_id = p_id)
       AND (p_after IS NULL OR id > p_after)
     ORDER BY id
     LIMIT p_limit
$$;

CREATE FUNCTION user_by_external(p_source text, p_id text) RETURNS SETOF users
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public
AS $$ SELECT * FROM users WHERE external_source = p_source AND external_id = p_id $$;

-- Creates the user mirror row if it does not exist (safe under concurrency)
-- and returns it with whether it was created.
CREATE FUNCTION ensure_user(p_new_id uuid, p_source text, p_id text, p_display_name text, p_provisioned text)
RETURNS TABLE (id uuid, created boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO users (id, external_source, external_id, display_name, provisioned)
    VALUES (p_new_id, p_source, p_id, p_display_name, p_provisioned)
    ON CONFLICT (external_source, external_id) WHERE external_id IS NOT NULL DO NOTHING;
    RETURN QUERY
        SELECT u.id, u.id = p_new_id FROM users u
         WHERE u.external_source = p_source AND u.external_id = p_id;
END
$$;

-- Changes a user's status and optionally its display name. Returns the row
-- before and after, for the audit event.
CREATE FUNCTION update_user(p_id uuid, p_status text, p_set_display boolean, p_display_name text)
RETURNS TABLE (before jsonb, after jsonb)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    b users;
    a users;
BEGIN
    SELECT * INTO b FROM users WHERE users.id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    UPDATE users
       SET status       = coalesce(p_status, users.status),
           display_name = CASE WHEN p_set_display THEN p_display_name ELSE users.display_name END,
           updated_at   = CASE WHEN coalesce(p_status, users.status) IS DISTINCT FROM users.status
                                 OR (p_set_display AND p_display_name IS DISTINCT FROM users.display_name)
                               THEN now() ELSE users.updated_at END
     WHERE users.id = p_id
    RETURNING * INTO a;
    before := jsonb_build_object('status', b.status, 'display_name', b.display_name);
    after  := jsonb_build_object('status', a.status, 'display_name', a.display_name);
    RETURN NEXT;
END
$$;

CREATE FUNCTION user_by_id(p_id uuid) RETURNS SETOF users
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public
AS $$ SELECT * FROM users WHERE id = p_id $$;

-- Records use of a key, including platform keys that no tenant can see.
CREATE FUNCTION touch_api_key(p_id uuid) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$ UPDATE api_keys SET last_used_at = now() WHERE id = p_id $$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION platform_tenant_id(), tenant_by_id(uuid), tenant_by_external(text, text),
    list_tenants(text, text, uuid, integer), user_by_external(text, text),
    ensure_user(uuid, text, text, text, text), update_user(uuid, text, boolean, text),
    user_by_id(uuid), touch_api_key(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform_tenant_id(), tenant_by_id(uuid), tenant_by_external(text, text),
    list_tenants(text, text, uuid, integer), user_by_external(text, text),
    ensure_user(uuid, text, text, text, text), update_user(uuid, text, boolean, text),
    user_by_id(uuid), touch_api_key(uuid) TO monitor_app, monitor_system;

-- Tenant rows and memberships are written by the api role inside a
-- transaction scoped to that tenant, so RLS checks every write.
GRANT INSERT, UPDATE ON tenants TO monitor_app;
GRANT INSERT, UPDATE, DELETE ON tenant_members TO monitor_app;

-- +goose Down
REVOKE INSERT, UPDATE, DELETE ON tenant_members FROM monitor_app;
REVOKE INSERT, UPDATE ON tenants FROM monitor_app;
DROP FUNCTION touch_api_key(uuid);
DROP FUNCTION user_by_id(uuid);
DROP FUNCTION update_user(uuid, text, boolean, text);
DROP FUNCTION ensure_user(uuid, text, text, text, text);
DROP FUNCTION user_by_external(text, text);
DROP FUNCTION list_tenants(text, text, uuid, integer);
DROP FUNCTION tenant_by_external(text, text);
DROP FUNCTION tenant_by_id(uuid);
DROP FUNCTION platform_tenant_id();
