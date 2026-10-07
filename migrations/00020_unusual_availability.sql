-- Availability "unusual" (2026-10-07): a device whose host check is in
-- PROBLEM only because of its Whoopsy! band works, just not as usual; it is
-- not shown as down.

-- +goose Up
CREATE OR REPLACE VIEW devices_v AS
SELECT d.id, d.tenant_id, t.name AS tenant_name, d.name, d.type, d.address, d.site_id, st.name AS site_name,
       d.parent_id, d.tags,
       CASE
           WHEN hc.id IS NULL THEN 'unmonitored'
           WHEN NOT hc.enabled THEN 'disabled'
           WHEN hs.check_id IS NULL OR hs.status = 'UNKNOWN' THEN 'unknown'
           WHEN hs.phase = 'PROBLEM' AND hs.machine->'whoopsy'->>'level' IS NOT NULL AND hs.last_status = 'OK'
                AND NOT jsonb_path_exists(hs.machine, '$.rules.*.level') THEN 'unusual'
           WHEN hs.phase = 'PROBLEM' THEN 'down'
           ELSE 'up' END AS availability,
       (SELECT count(*) FROM incidents i WHERE i.device_id = d.id AND i.status <> 'resolved') AS open_incidents
  FROM devices d
  JOIN tenants t ON t.id = d.tenant_id
  LEFT JOIN sites st ON st.id = d.site_id
  LEFT JOIN checks hc ON hc.device_id = d.id AND hc.is_host_check
  LEFT JOIN check_state hs ON hs.check_id = hc.id;

-- +goose Down
CREATE OR REPLACE VIEW devices_v AS
SELECT d.id, d.tenant_id, t.name AS tenant_name, d.name, d.type, d.address, d.site_id, st.name AS site_name,
       d.parent_id, d.tags,
       CASE
           WHEN hc.id IS NULL THEN 'unmonitored'
           WHEN NOT hc.enabled THEN 'disabled'
           WHEN hs.check_id IS NULL OR hs.status = 'UNKNOWN' THEN 'unknown'
           WHEN hs.phase = 'PROBLEM' THEN 'down'
           ELSE 'up' END AS availability,
       (SELECT count(*) FROM incidents i WHERE i.device_id = d.id AND i.status <> 'resolved') AS open_incidents
  FROM devices d
  JOIN tenants t ON t.id = d.tenant_id
  LEFT JOIN sites st ON st.id = d.site_id
  LEFT JOIN checks hc ON hc.device_id = d.id AND hc.is_host_check
  LEFT JOIN check_state hs ON hs.check_id = hc.id;
