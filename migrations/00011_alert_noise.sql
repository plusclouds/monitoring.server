-- Alert noise control (M4): dependency suppression (F05), route grouping
-- and repeat notifications (F06).

-- +goose Up

-- The device whose failure explains an incident: its own device, or the
-- device of the root incident when it is suppressed. Routes group by it.
ALTER TABLE incidents ADD COLUMN root_device_id uuid;
UPDATE incidents SET root_device_id = device_id;
CREATE INDEX incidents_root ON incidents (root_incident_id) WHERE root_incident_id IS NOT NULL AND status <> 'resolved';

-- Events are routed in order; route_after holds an event (and the later
-- events of its incident) back, for the dependency grace period.
ALTER TABLE events ADD COLUMN route_after timestamptz;
CREATE INDEX events_unrouted_subject ON events (subject, seq) WHERE routed_at IS NULL;

ALTER TABLE alert_routes
    ADD COLUMN group_by                text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN group_wait_seconds      integer NOT NULL DEFAULT 30 CHECK (group_wait_seconds BETWEEN 0 AND 600),
    ADD COLUMN repeat_interval_seconds integer CHECK (repeat_interval_seconds BETWEEN 300 AND 604800);

-- Events of a grouping route collect here until group_wait has passed;
-- then they are sent as one event carrying all of them (F06).
CREATE TABLE alert_groups (
    id          uuid PRIMARY KEY,
    tenant_id   uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    route_id    uuid        NOT NULL,
    endpoint_id uuid        NOT NULL,
    event_type  text        NOT NULL,
    group_key   jsonb       NOT NULL,   -- {group_by field: value}
    flush_at    timestamptz NOT NULL,
    flushed_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, route_id) REFERENCES alert_routes (tenant_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX alert_groups_open ON alert_groups (route_id, event_type, group_key) WHERE flushed_at IS NULL;
CREATE INDEX alert_groups_due ON alert_groups (flush_at) WHERE flushed_at IS NULL;

CREATE TABLE alert_group_events (
    group_id  uuid   NOT NULL REFERENCES alert_groups (id) ON DELETE CASCADE,
    event_id  uuid   NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    event_seq bigint NOT NULL,
    PRIMARY KEY (group_id, event_id)
);

-- When each route last notified an incident, for repeat_interval.
CREATE TABLE route_notifications (
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    route_id     uuid        NOT NULL,
    incident_id  uuid        NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    last_sent_at timestamptz NOT NULL,
    PRIMARY KEY (route_id, incident_id),
    FOREIGN KEY (tenant_id, route_id) REFERENCES alert_routes (tenant_id, id) ON DELETE CASCADE
);

ALTER TABLE alert_groups ENABLE ROW LEVEL SECURITY;
ALTER TABLE route_notifications ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON alert_groups
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON route_notifications
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON alert_groups, alert_group_events, route_notifications TO monitor_system;
GRANT SELECT ON alert_groups, route_notifications TO monitor_app;

-- +goose Down
DROP TABLE route_notifications, alert_group_events, alert_groups;
ALTER TABLE alert_routes DROP COLUMN repeat_interval_seconds, DROP COLUMN group_wait_seconds, DROP COLUMN group_by;
DROP INDEX events_unrouted_subject;
ALTER TABLE events DROP COLUMN route_after;
DROP INDEX incidents_root;
ALTER TABLE incidents DROP COLUMN root_device_id;
