-- Collectors (M4, F05, ADR-0014): the objects a collector reports
-- (interfaces, fans, disks) with their own state, and incidents per object.

-- +goose Up

-- One row per object of a collector check: what it is and its state. An
-- object missing from a successful run gets gone_at; its incident resolves.
CREATE TABLE check_objects (
    check_id       uuid        NOT NULL,
    object_key     text        NOT NULL CHECK (length(object_key) BETWEEN 1 AND 200),
    tenant_id      uuid        NOT NULL,
    name           text        NOT NULL,
    labels         jsonb       NOT NULL DEFAULT '{}',
    phase          text        NOT NULL CHECK (phase IN ('OK', 'PENDING', 'PROBLEM')),
    status         text        NOT NULL CHECK (status IN ('OK', 'WARNING', 'CRITICAL', 'UNKNOWN')),
    since          timestamptz NOT NULL,
    machine        jsonb       NOT NULL,       -- engine.State
    last_status    text,                       -- the plugin's own status of the object
    last_output    text,
    last_metrics   jsonb       NOT NULL DEFAULT '{}',
    incident_id    uuid,
    first_seen_at  timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL,
    gone_at        timestamptz,
    PRIMARY KEY (check_id, object_key),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX check_objects_tenant ON check_objects (tenant_id);
CREATE INDEX check_objects_gone ON check_objects (gone_at) WHERE gone_at IS NOT NULL;

ALTER TABLE check_objects ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON check_objects
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON check_objects TO monitor_system;
GRANT SELECT, DELETE ON check_objects TO monitor_app;

-- Incidents of an object carry its key and name; a check's own incident has
-- neither. One unresolved incident per check and object.
ALTER TABLE incidents ADD COLUMN object_key text, ADD COLUMN object_name text;
DROP INDEX incidents_one_active;
CREATE UNIQUE INDEX incidents_one_active ON incidents (check_id, coalesce(object_key, ''))
    WHERE status <> 'resolved' AND check_id IS NOT NULL;
ALTER TABLE incidents DROP CONSTRAINT incidents_resolved_by_check;
ALTER TABLE incidents ADD CONSTRAINT incidents_resolved_by_check
    CHECK (resolved_by IN ('recovery', 'manual', 'check-deleted', 'check-disabled', 'object-gone'));

-- Grafana: object names and incident objects (new columns at the end).
CREATE OR REPLACE VIEW metric_series_v AS
SELECT s.id AS series_id, s.tenant_id, g.device_id, d.name AS device_name, g.source_id AS check_id,
       c.name AS check_name, g.plugin, g.object_key AS object, s.name, s.unit, s.kind, s.labels,
       rc.name AS retention_class, o.name AS object_name
  FROM metric_series s
  JOIN metric_groups g ON g.id = s.group_id
  JOIN retention_classes rc ON rc.id = g.retention_class
  LEFT JOIN devices d ON d.id = g.device_id
  LEFT JOIN checks c ON c.id = g.source_id
  LEFT JOIN check_objects o ON o.check_id = g.source_id AND o.object_key = g.object_key AND g.object_key <> '';

CREATE OR REPLACE VIEW incidents_v AS
SELECT i.id, i.tenant_id, i.device_id, d.name AS device_name, i.check_id, c.name AS check_name, i.severity,
       i.status, i.summary, i.flapping, i.opened_at, i.acknowledged_at, i.resolved_at, i.resolved_by,
       coalesce(i.resolved_at, now()) - i.opened_at AS duration, i.object_key, i.object_name
  FROM incidents i
  LEFT JOIN devices d ON d.id = i.device_id
  LEFT JOIN checks c ON c.id = i.check_id;

-- +goose Down
DROP VIEW incidents_v;
DROP VIEW metric_series_v;
CREATE VIEW metric_series_v AS
SELECT s.id AS series_id, s.tenant_id, g.device_id, d.name AS device_name, g.source_id AS check_id,
       c.name AS check_name, g.plugin, g.object_key AS object, s.name, s.unit, s.kind, s.labels,
       rc.name AS retention_class
  FROM metric_series s
  JOIN metric_groups g ON g.id = s.group_id
  JOIN retention_classes rc ON rc.id = g.retention_class
  LEFT JOIN devices d ON d.id = g.device_id
  LEFT JOIN checks c ON c.id = g.source_id;
CREATE VIEW incidents_v AS
SELECT i.id, i.tenant_id, i.device_id, d.name AS device_name, i.check_id, c.name AS check_name, i.severity,
       i.status, i.summary, i.flapping, i.opened_at, i.acknowledged_at, i.resolved_at, i.resolved_by,
       coalesce(i.resolved_at, now()) - i.opened_at AS duration
  FROM incidents i
  LEFT JOIN devices d ON d.id = i.device_id
  LEFT JOIN checks c ON c.id = i.check_id;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'monitor_grafana') THEN
        GRANT SELECT ON metric_series_v, incidents_v TO monitor_grafana;
    END IF;
END $$;
-- +goose StatementEnd
UPDATE incidents SET resolved_by = 'recovery' WHERE resolved_by = 'object-gone';
ALTER TABLE incidents DROP CONSTRAINT incidents_resolved_by_check;
ALTER TABLE incidents ADD CONSTRAINT incidents_resolved_by_check
    CHECK (resolved_by IN ('recovery', 'manual', 'check-deleted', 'check-disabled'));
DELETE FROM incidents WHERE object_key IS NOT NULL;
DROP INDEX incidents_one_active;
CREATE UNIQUE INDEX incidents_one_active ON incidents (check_id) WHERE status <> 'resolved' AND check_id IS NOT NULL;
ALTER TABLE incidents DROP COLUMN object_name, DROP COLUMN object_key;
DROP TABLE check_objects;
