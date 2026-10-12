-- Link webhook deliveries to the devices they are about, so a device can list
-- its own notifications without scanning event envelopes (F06).
--
-- One row per delivery and device: a plain incident event has one device, a
-- grouped event has one per grouped incident. Tenant events (limits) have none.
-- A trigger fills the table for every new delivery, so no code path that
-- creates deliveries has to remember it.

-- +goose Up
CREATE TABLE webhook_delivery_devices (
    delivery_id uuid NOT NULL REFERENCES webhook_deliveries (id) ON DELETE CASCADE,
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    device_id   uuid NOT NULL,
    PRIMARY KEY (delivery_id, device_id)
);
-- Newest first: delivery IDs are UUIDv7.
CREATE INDEX webhook_delivery_devices_device ON webhook_delivery_devices (tenant_id, device_id, delivery_id DESC);

ALTER TABLE webhook_delivery_devices ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_delivery_devices
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_delivery_devices TO monitor_system;
GRANT SELECT ON webhook_delivery_devices TO monitor_app;

-- +goose StatementBegin
CREATE FUNCTION link_delivery_devices() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO webhook_delivery_devices (delivery_id, tenant_id, device_id)
    SELECT NEW.id, NEW.tenant_id, x.device::uuid
      FROM (
        SELECT e.envelope #>> '{data,device,id}' AS device
          FROM events e WHERE e.id = NEW.event_id
        UNION
        SELECT o #>> '{device,id}'
          FROM events e,
               jsonb_array_elements(CASE WHEN jsonb_typeof(e.envelope #> '{data,objects}') = 'array'
                                         THEN e.envelope #> '{data,objects}' ELSE '[]'::jsonb END) o
         WHERE e.id = NEW.event_id
      ) x
     WHERE x.device IS NOT NULL
    ON CONFLICT DO NOTHING;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER link_delivery_devices AFTER INSERT ON webhook_deliveries
    FOR EACH ROW EXECUTE FUNCTION link_delivery_devices();

-- Deliveries that already exist (kept by delivery_retention, 30 days by default).
INSERT INTO webhook_delivery_devices (delivery_id, tenant_id, device_id)
SELECT d.id, d.tenant_id, x.device::uuid
  FROM webhook_deliveries d
  JOIN events e ON e.id = d.event_id
  CROSS JOIN LATERAL (
        SELECT e.envelope #>> '{data,device,id}' AS device
        UNION
        SELECT o #>> '{device,id}'
          FROM jsonb_array_elements(CASE WHEN jsonb_typeof(e.envelope #> '{data,objects}') = 'array'
                                         THEN e.envelope #> '{data,objects}' ELSE '[]'::jsonb END) o
  ) x
 WHERE x.device IS NOT NULL
ON CONFLICT DO NOTHING;

-- +goose Down
DROP TRIGGER link_delivery_devices ON webhook_deliveries;
DROP FUNCTION link_delivery_devices();
DROP TABLE webhook_delivery_devices;
