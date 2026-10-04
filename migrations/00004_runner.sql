-- Change notifications for runners (F04, ADR-0002).
--
-- Any change to what runs (checks, their credentials, devices, tenant
-- status) sends NOTIFY config_changed with the tenant ID. PostgreSQL folds
-- identical notifications within a transaction, so a bulk change is one
-- notification per tenant. Runners also resync on a timer, so a missed
-- notification only delays a change.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION notify_config_changed() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    tenant uuid;
BEGIN
    IF TG_TABLE_NAME = 'tenants' THEN
        tenant := coalesce(NEW.id, OLD.id);
    ELSIF TG_OP = 'DELETE' THEN
        tenant := OLD.tenant_id;
    ELSE
        tenant := NEW.tenant_id;
    END IF;
    PERFORM pg_notify('config_changed', tenant::text);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER notify_config_changed AFTER INSERT OR UPDATE OR DELETE ON checks
    FOR EACH ROW EXECUTE FUNCTION notify_config_changed();
CREATE TRIGGER notify_config_changed AFTER INSERT OR UPDATE OR DELETE ON check_credentials
    FOR EACH ROW EXECUTE FUNCTION notify_config_changed();
CREATE TRIGGER notify_config_changed AFTER INSERT OR UPDATE OR DELETE ON devices
    FOR EACH ROW EXECUTE FUNCTION notify_config_changed();
CREATE TRIGGER notify_config_changed AFTER UPDATE OF status ON tenants
    FOR EACH ROW EXECUTE FUNCTION notify_config_changed();

-- +goose Down
DROP TRIGGER notify_config_changed ON tenants;
DROP TRIGGER notify_config_changed ON devices;
DROP TRIGGER notify_config_changed ON check_credentials;
DROP TRIGGER notify_config_changed ON checks;
DROP FUNCTION notify_config_changed();
