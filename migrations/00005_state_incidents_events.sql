-- Check state, incidents and the event log (F05, ADR-0009).
--
-- events is the transactional outbox: the engine and the API write an event
-- in the same transaction as the incident change it describes. The notifier
-- routes events to webhook endpoints (F06); the SSE stream replays them.

-- +goose Up

CREATE TABLE check_state (
    check_id       uuid PRIMARY KEY,
    tenant_id      uuid        NOT NULL,
    phase          text        NOT NULL CHECK (phase IN ('OK', 'PENDING', 'PROBLEM')),
    status         text        NOT NULL CHECK (status IN ('OK', 'WARNING', 'CRITICAL', 'UNKNOWN')),
    since          timestamptz NOT NULL,
    machine        jsonb       NOT NULL,       -- engine.State: counters, flap history, rule levels
    last_result_at timestamptz,
    last_status    text,                       -- the plugin's own status of the last result
    last_output    text,
    last_metrics   jsonb       NOT NULL DEFAULT '{}',
    incident_id    uuid,
    version        bigint      NOT NULL DEFAULT 1,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, check_id),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX check_state_stale ON check_state (last_result_at);

CREATE TABLE incidents (
    id                     uuid PRIMARY KEY,
    tenant_id              uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    check_id               uuid,                         -- null once the check is deleted
    device_id              uuid,
    severity               text        NOT NULL CHECK (severity IN ('warning', 'critical')),
    status                 text        NOT NULL CHECK (status IN ('open', 'acknowledged', 'resolved')),
    summary                text        NOT NULL,
    last_output            text,
    rule_id                text,
    rule_name              text,
    flapping               boolean     NOT NULL DEFAULT false,
    suppressed             boolean     NOT NULL DEFAULT false,
    root_incident_id       uuid,
    opened_at              timestamptz NOT NULL,
    acknowledged_at        timestamptz,
    acknowledged_by        uuid,                         -- users.id
    acknowledged_by_ext    text,
    resolved_at            timestamptz,
    resolved_by            text CHECK (resolved_by IN ('recovery', 'manual', 'check-deleted', 'check-disabled')),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE SET NULL (check_id),
    FOREIGN KEY (tenant_id, device_id) REFERENCES devices (tenant_id, id) ON DELETE SET NULL (device_id)
);
CREATE INDEX incidents_active ON incidents (tenant_id, opened_at DESC) WHERE status <> 'resolved';
CREATE INDEX incidents_tenant ON incidents (tenant_id, id DESC);
CREATE INDEX incidents_check ON incidents (check_id) WHERE check_id IS NOT NULL;
-- At most one unresolved incident per check.
CREATE UNIQUE INDEX incidents_one_active ON incidents (check_id) WHERE status <> 'resolved' AND check_id IS NOT NULL;

CREATE TABLE incident_comments (
    id                 uuid PRIMARY KEY,
    tenant_id          uuid        NOT NULL,
    incident_id        uuid        NOT NULL,
    author_kind        text        NOT NULL,
    author_user_id     uuid,
    author_external_id text,
    body               text        NOT NULL CHECK (length(body) BETWEEN 1 AND 10000),
    created_at         timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES incidents (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX incident_comments_incident ON incident_comments (incident_id, created_at);

-- CloudEvents 1.0 envelopes (ADR-0009), one per event, in the tenant's order.
CREATE TABLE events (
    id         uuid PRIMARY KEY,
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    seq        bigint      GENERATED ALWAYS AS IDENTITY,
    type       text        NOT NULL,
    subject    text,
    time       timestamptz NOT NULL,
    envelope   jsonb       NOT NULL,
    routed_at  timestamptz,
    UNIQUE (seq)
);
CREATE INDEX events_unrouted ON events (seq) WHERE routed_at IS NULL;
CREATE INDEX events_tenant ON events (tenant_id, seq);

-- Tell notifiers and SSE streams that events are waiting.
-- +goose StatementBegin
CREATE FUNCTION notify_event() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public
AS $$
BEGIN
    PERFORM pg_notify('events', NEW.tenant_id::text);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER notify_event AFTER INSERT ON events FOR EACH ROW EXECUTE FUNCTION notify_event();

ALTER TABLE check_state       ENABLE ROW LEVEL SECURITY;
ALTER TABLE incidents         ENABLE ROW LEVEL SECURITY;
ALTER TABLE incident_comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE events            ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON check_state
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON incidents
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON incident_comments
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON events
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The api role acknowledges, resolves and comments (with events); the
-- engine (system role) does everything else.
GRANT SELECT, UPDATE, DELETE ON check_state TO monitor_app;
GRANT SELECT, UPDATE ON incidents TO monitor_app;
GRANT SELECT, INSERT ON incident_comments, events TO monitor_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON check_state, incidents, incident_comments, events TO monitor_system;

-- +goose Down
DROP TABLE events;
DROP FUNCTION notify_event();
DROP TABLE incident_comments;
DROP TABLE incidents;
DROP TABLE check_state;
