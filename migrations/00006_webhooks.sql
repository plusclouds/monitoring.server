-- Webhook endpoints, alert routes and deliveries (F06, ADR-0009).
--
-- The notifier routes each event (00005) to the endpoints of matching
-- routes, one delivery row per endpoint, and delivers them signed with
-- retries. Deliveries are the per-endpoint outbox.

-- +goose Up

-- secrets holds the signing secret, the previous one during rotation and
-- the static extra headers, as one encrypted JSON object (ADR-0006).
CREATE TABLE webhook_endpoints (
    id                   uuid PRIMARY KEY,
    tenant_id            uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name                 text        NOT NULL,
    url                  text        NOT NULL,
    enabled              boolean     NOT NULL DEFAULT true,
    disabled_reason      text,
    timeout_seconds      integer     NOT NULL DEFAULT 10 CHECK (timeout_seconds BETWEEN 1 AND 30),
    header_names         text[]      NOT NULL DEFAULT '{}',
    ciphertext           bytea       NOT NULL,
    nonce                bytea       NOT NULL,
    wrapped_dek          bytea       NOT NULL,
    kek_id               text        NOT NULL,
    previous_expires_at  timestamptz,
    managed_by           text        NOT NULL DEFAULT 'api',
    external_source      text,
    external_type        text,
    external_id          text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name),
    CHECK ((external_source IS NULL) = (external_id IS NULL))
);
CREATE UNIQUE INDEX webhook_endpoints_external
    ON webhook_endpoints (tenant_id, external_source, coalesce(external_type, ''), external_id)
    WHERE external_id IS NOT NULL;

CREATE TABLE alert_routes (
    id              uuid PRIMARY KEY,
    tenant_id       uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name            text        NOT NULL,
    position        integer     NOT NULL,
    enabled         boolean     NOT NULL DEFAULT true,
    match           jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(match) = 'object'),
    endpoint_id     uuid        NOT NULL,
    continue        boolean     NOT NULL DEFAULT false,
    labels          jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(labels) = 'object'),
    managed_by      text        NOT NULL DEFAULT 'api',
    external_source text,
    external_type   text,
    external_id     text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name),
    CHECK ((external_source IS NULL) = (external_id IS NULL)),
    FOREIGN KEY (tenant_id, endpoint_id) REFERENCES webhook_endpoints (tenant_id, id)
);
CREATE INDEX alert_routes_order ON alert_routes (tenant_id, position, id);
CREATE UNIQUE INDEX alert_routes_external
    ON alert_routes (tenant_id, external_source, coalesce(external_type, ''), external_id)
    WHERE external_id IS NOT NULL;

CREATE TABLE webhook_deliveries (
    id               uuid PRIMARY KEY,
    tenant_id        uuid        NOT NULL,
    event_id         uuid        NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    event_seq        bigint      NOT NULL,
    event_type       text        NOT NULL,
    subject          text        NOT NULL DEFAULT '',  -- incident ID: deliveries of one subject go in order
    endpoint_id      uuid        NOT NULL,
    route_id         uuid,
    route            jsonb       NOT NULL DEFAULT 'null',  -- data.route of the delivered body
    status           text        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'delivered', 'failed', 'cancelled')),
    attempts         integer     NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_attempt_at  timestamptz,
    last_status_code integer,
    last_error       text,
    last_response    text,
    delivered_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id),
    FOREIGN KEY (tenant_id, endpoint_id) REFERENCES webhook_endpoints (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, route_id) REFERENCES alert_routes (tenant_id, id) ON DELETE SET NULL (route_id)
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_order ON webhook_deliveries (endpoint_id, subject, event_seq) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint ON webhook_deliveries (endpoint_id, id DESC);

ALTER TABLE webhook_endpoints  ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_routes       ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON webhook_endpoints
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON alert_routes
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON webhook_deliveries
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_endpoints, alert_routes TO monitor_app, monitor_system;
GRANT SELECT, UPDATE ON webhook_deliveries TO monitor_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_deliveries TO monitor_system;

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE alert_routes;
DROP TABLE webhook_endpoints;
