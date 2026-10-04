-- Webhook limits per tenant and delivery retention (F06, v0.4.1).

-- +goose Up
ALTER TABLE tenants
    ADD COLUMN max_webhooks     integer NOT NULL DEFAULT 20 CHECK (max_webhooks >= 0),
    ADD COLUMN max_alert_routes integer NOT NULL DEFAULT 50 CHECK (max_alert_routes >= 0);
UPDATE tenants SET max_webhooks = 1000, max_alert_routes = 1000 WHERE is_platform;

CREATE INDEX webhook_deliveries_created ON webhook_deliveries (created_at) WHERE status <> 'pending';
CREATE INDEX events_routed ON events (time) WHERE routed_at IS NOT NULL;
GRANT DELETE ON events, webhook_deliveries TO monitor_system;

-- +goose Down
DROP INDEX events_routed;
DROP INDEX webhook_deliveries_created;
ALTER TABLE tenants DROP COLUMN max_alert_routes, DROP COLUMN max_webhooks;
