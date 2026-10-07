-- Grafana: the objects of collector checks (interfaces, fans, VMs) with
-- their state, for the M4 dashboards.

-- +goose Up
CREATE VIEW check_objects_v AS
SELECT o.check_id, o.tenant_id, c.device_id, d.name AS device_name, c.name AS check_name, c.plugin,
       o.object_key, o.name, o.labels, o.phase, o.status, o.since, o.last_status, o.last_output, o.last_metrics,
       o.device_id AS object_device_id, od.name AS object_device_name, o.incident_id, o.last_seen_at, o.gone_at
  FROM check_objects o
  JOIN checks c ON c.id = o.check_id
  JOIN devices d ON d.id = c.device_id
  LEFT JOIN devices od ON od.id = o.device_id;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'monitor_grafana') THEN
        GRANT SELECT ON check_objects_v TO monitor_grafana;
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP VIEW check_objects_v;
