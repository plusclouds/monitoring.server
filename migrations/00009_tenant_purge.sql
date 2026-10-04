-- Restoring and purging deleted tenants (F01, v0.3.1).
--
-- A deleted tenant can be restored until tenant_purge_grace has passed.
-- After that the maintenance role purges it: every row it owns is removed
-- except its audit log, its external ID is released for reuse, and the
-- tenant row stays as a 'purged' tombstone so audit events keep their
-- tenant. Metric samples and rollups carry no tenant; they expire with
-- retention once their series are gone.

-- +goose Up
ALTER TABLE tenants DROP CONSTRAINT tenants_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_check
    CHECK (status IN ('active', 'suspended', 'deleted', 'purged'));
ALTER TABLE tenants ADD COLUMN purged_at timestamptz;

-- +goose StatementBegin
-- Removes the data of a deleted tenant and returns its former external ID.
-- Refuses any tenant that is not deleted.
CREATE FUNCTION purge_tenant(p_id uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    ext text;
BEGIN
    SELECT external_id INTO ext FROM public.tenants WHERE id = p_id AND status = 'deleted' FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'tenant % is not deleted', p_id;
    END IF;
    DELETE FROM public.webhook_deliveries WHERE tenant_id = p_id;
    DELETE FROM public.alert_routes WHERE tenant_id = p_id;
    DELETE FROM public.webhook_endpoints WHERE tenant_id = p_id;
    DELETE FROM public.incident_comments WHERE tenant_id = p_id;
    DELETE FROM public.events WHERE tenant_id = p_id;
    DELETE FROM public.check_state WHERE tenant_id = p_id;
    DELETE FROM public.incidents WHERE tenant_id = p_id;
    DELETE FROM public.check_credentials WHERE tenant_id = p_id;
    DELETE FROM public.checks WHERE tenant_id = p_id;
    DELETE FROM public.credentials WHERE tenant_id = p_id;
    DELETE FROM public.device_dependencies WHERE tenant_id = p_id;
    UPDATE public.devices SET parent_id = NULL, physical_peer_id = NULL WHERE tenant_id = p_id;
    DELETE FROM public.devices WHERE tenant_id = p_id;
    DELETE FROM public.sites WHERE tenant_id = p_id;
    DELETE FROM public.metric_groups WHERE tenant_id = p_id;
    DELETE FROM public.api_keys WHERE tenant_id = p_id;
    DELETE FROM public.tenant_members WHERE tenant_id = p_id;
    UPDATE public.tenants
       SET status = 'purged', purged_at = now(), updated_at = now(),
           external_source = NULL, external_type = NULL, external_id = NULL
     WHERE id = p_id;
    RETURN ext;
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION purge_tenant(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION purge_tenant(uuid) TO monitor_system;

-- +goose Down
DROP FUNCTION purge_tenant(uuid);
ALTER TABLE tenants DROP COLUMN purged_at;
UPDATE tenants SET status = 'deleted' WHERE status = 'purged';
ALTER TABLE tenants DROP CONSTRAINT tenants_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_check CHECK (status IN ('active', 'suspended', 'deleted'));
