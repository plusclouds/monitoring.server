-- Escalation steps of alert routes (F06).
--
-- A route's steps after the first are scheduled per incident when the
-- incident is notified; the notifier sends each one when it is due, as
-- monitoring.incident.escalated with that step's labels, unless the
-- incident was resolved (or acknowledged, for steps that ask for it).

-- +goose Up
ALTER TABLE alert_routes ADD COLUMN steps jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(steps) = 'array');

CREATE TABLE route_escalations (
    tenant_id   uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    route_id    uuid        NOT NULL,
    incident_id uuid        NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    step        integer     NOT NULL CHECK (step >= 1),
    due_at      timestamptz NOT NULL,
    done_at     timestamptz,
    outcome     text CHECK (outcome IN ('sent', 'skipped-acknowledged', 'skipped-resolved', 'skipped-suppressed')),
    PRIMARY KEY (route_id, incident_id, step),
    FOREIGN KEY (tenant_id, route_id) REFERENCES alert_routes (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX route_escalations_due ON route_escalations (due_at) WHERE done_at IS NULL;

ALTER TABLE route_escalations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON route_escalations
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON route_escalations TO monitor_system;
GRANT SELECT ON route_escalations TO monitor_app;

-- +goose Down
DROP TABLE route_escalations;
ALTER TABLE alert_routes DROP COLUMN steps;
