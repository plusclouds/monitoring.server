# Notification catalog

The same list is served by `GET /v1/notification-catalog`. `GET /v1/devices/{device_id}/notifications` shows one device's routes, webhook health and recent deliveries.

What a customer can be notified about, what each notification looks like, and which alert-route settings control it. Sources: [F06](../features/F06-notifications.md), [ADR-0009](../adr/0009-webhook-delivery.md), `internal/incident/events.go`, `internal/webhook/routes.go`, `api/openapi.yaml`.

## 1. Notifications

| Event type | When it is sent | Delivered by default | Goes to | Stops when |
| --- | --- | --- | --- | --- |
| `monitoring.incident.opened` | A check crosses a threshold and an incident opens. Also sent when a suppressed incident stops being explained by its upstream failure. | Yes | Every matching route | n/a |
| `monitoring.incident.updated` | Severity changes (warning to critical or back) or the incident starts or stops flapping. | Yes | Every matching route | n/a |
| `monitoring.incident.acknowledged` | A user acknowledges the incident. | Yes | Every matching route | n/a |
| `monitoring.incident.resolved` | The check recovers, or the incident is resolved by a user, a deleted or disabled check, or a vanished object. | Yes | Every matching route | n/a |
| `monitoring.incident.renotify` | An incident is still open and unacknowledged after the route's `repeat_interval`. | Only if the route sets `repeat_interval_seconds` (300 to 604800) | The route that sent the original | Acknowledge or resolve |
| `monitoring.incident.escalated` | A later route step is due (`steps[n].after`). | Only if the route has more than one step | The route that owns the step | Resolved, suppressed, or acknowledged when the step sets `only_if_unacknowledged` |
| `monitoring.incident.commented` | A comment is added to an incident. | No. The route must list it in `match.event_types`. | Routes that list it | n/a |
| `monitoring.tenant.device_limit_reached` | A create brings the account to its device limit. | Yes, but only to routes with no device, check or severity filter | Routes without those filters | Never grouped, repeated or escalated |
| `monitoring.tenant.check_limit_reached` | A create brings the account to its check limit. | Same as above | Same as above | Same as above |
| `monitoring.webhook.test` | `POST /v1/webhooks/{id}/test`. | Sent on request to one endpoint | That endpoint | n/a |
| `monitoring.heartbeat` | The engine's own dead-man's switch (F11). Sent to operator-configured targets, not to tenant webhooks. | n/a | Operator targets | n/a |

Not notified: suppressed incidents (an upstream failure explains them), unless they later become unsuppressed.

## 2. Envelope (every event)

CloudEvents 1.0, signed with Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`). Receivers deduplicate on `webhook-id`.

| Field | Meaning |
| --- | --- |
| `specversion` | Always `1.0` |
| `id` | UUIDv7. Same on every retry and equal to the `webhook-id` header. |
| `source` | `/monitoring/<installation_id>/<tenant_id>` |
| `type` | Event type from the table above |
| `time` | RFC 3339, UTC |
| `subject` | Incident ID. For a grouped event, `group:<group id>`. For a tenant event, `tenant:<id>`. |
| `data.account_id` | PlusClouds account UUID, or `null` when standalone |
| `data.object_type` | `Monitoring\Incidents` (tenant events: `Monitoring\Tenants`) |
| `data.object` | The incident (section 3) |
| `data.device` | The device (section 4) |
| `data.check` | The check (section 5) |
| `data.route` | The route, step and labels that sent this delivery (section 6) |
| `data.actor` | Who acknowledged, resolved or commented. `null` otherwise. |
| `data.links.incident` | Panel link, only when the operator configured `notifier.incident_url_template` |

## 3. Incident object (`data.object`)

| Field | Type | Notes |
| --- | --- | --- |
| `id` | uuid | |
| `check_id`, `device_id` | uuid or null | |
| `object_key`, `object_name` | string or null | For collector checks, the interface or fan the incident is about. Null for the check itself. |
| `severity` | `warning` or `critical` | |
| `status` | `open`, `acknowledged`, `resolved` | |
| `summary` | string | Ready-made one-line text for a message title |
| `last_output` | string or null | Last check output |
| `rule_id`, `rule_name` | string or null | The threshold rule that fired |
| `flapping` | bool | |
| `suppressed` | bool | Suppressed incidents are not notified |
| `root_incident_id`, `root_device_id` | uuid or null | The upstream cause, if any |
| `opened_at`, `acknowledged_at`, `resolved_at`, `updated_at` | RFC 3339 or null | |
| `acknowledged_by`, `acknowledged_by_external_id` | uuid / string or null | |
| `resolved_by` | `recovery`, `manual`, `check-deleted`, `check-disabled`, `object-gone` or null | |

## 4. Device (`data.device`)

`id`, `name`, `address`, `type`, `site_id`, `tags` (map), `external` (`{source, type, id}`, the PlusClouds link) and `path`, the containers outermost first, each with `id`, `name` and `external`. A receiver can map the event to its own objects without a lookup.

## 5. Check (`data.check`)

`id`, `name`, `plugin`, `last_output`, `rule_id`, `rule_name`, `runbook_url`, `thresholds` (the check's rules as in the API).

## 6. Route (`data.route`)

`id`, `name`, `step` (0 for the first notification), `labels` (the route's labels merged with the step's labels). The receiver decides what labels mean, for example `recipient: on-call`.

## 7. Example: incident opened

```json
{
  "specversion": "1.0",
  "id": "0192f7c4-5b1e-7a3c-9d2e-3f4a5b6c7d8e",
  "source": "/monitoring/inst-01/0192f7b0-0000-7000-8000-000000000001",
  "type": "monitoring.incident.opened",
  "time": "2026-10-12T09:15:00Z",
  "datacontenttype": "application/json",
  "subject": "0192f7c4-4a00-7000-8000-00000000a001",
  "data": {
    "account_id": "9b1c2d3e-0000-4000-8000-000000000001",
    "object_type": "Monitoring\\Incidents",
    "object": {
      "id": "0192f7c4-4a00-7000-8000-00000000a001",
      "check_id": "0192f7a0-0000-7000-8000-00000000c001",
      "device_id": "0192f7a0-0000-7000-8000-00000000d001",
      "object_key": null,
      "object_name": null,
      "severity": "critical",
      "status": "open",
      "summary": "CPU load 97 % (critical above 90 %)",
      "last_output": "load=97",
      "rule_id": "cpu-critical",
      "rule_name": "CPU critical",
      "flapping": false,
      "suppressed": false,
      "root_incident_id": null,
      "root_device_id": "0192f7a0-0000-7000-8000-00000000d001",
      "opened_at": "2026-10-12T09:15:00Z",
      "acknowledged_at": null,
      "resolved_at": null,
      "updated_at": "2026-10-12T09:15:00Z"
    },
    "device": {
      "id": "0192f7a0-0000-7000-8000-00000000d001",
      "name": "web-01",
      "address": "10.0.0.11",
      "type": "server",
      "site_id": "0192f7a0-0000-7000-8000-00000000e001",
      "tags": { "rack": "a12" },
      "external": { "source": "plusclouds", "type": "NextDeveloper\\IAAS\\Database\\Models\\VirtualMachines", "id": "9b1c…" },
      "path": [ { "id": "0192f7a0-0000-7000-8000-00000000b001", "name": "hv-07", "external": null } ]
    },
    "check": {
      "id": "0192f7a0-0000-7000-8000-00000000c001",
      "name": "CPU load",
      "plugin": "cpu",
      "last_output": "load=97",
      "rule_id": "cpu-critical",
      "rule_name": "CPU critical",
      "runbook_url": null,
      "thresholds": []
    },
    "route": { "id": "0192f7a0-0000-7000-8000-00000000f001", "name": "datacenter critical", "step": 0, "labels": { "recipient": "noc" } },
    "actor": null,
    "links": { "incident": "https://panel.example.com/monitoring/incidents/0192f7c4-4a00-7000-8000-00000000a001" }
  }
}
```

Values are illustrative. Field names and types come from the code and the OpenAPI schema.

## 8. How each event differs from the example

| Event | Change |
| --- | --- |
| `updated` | `object.severity` or `object.flapping` changed |
| `acknowledged` | `object.status = acknowledged`, `acknowledged_at` and `acknowledged_by*` set, `actor` filled |
| `resolved` | `object.status = resolved`, `resolved_at` and `resolved_by` set. Actor is filled for a manual resolve. |
| `renotify` | Same incident, still open. `data.route` is the route that repeats. |
| `escalated` | `data.route.step` is 1 or higher and `labels` are merged with the step's labels |
| `commented` | `actor` filled. `data.object.comment` holds the comment (`id`, `author_kind`, `author_user_id`, `author_external_id`, `body`, `created_at`). |
| Grouped (any incident type) | `subject = group:<id>`. `data.object`, `data.device`, `data.check` are `null`. `data.objects` is a list of `{object, device, check}`. `data.group = {id, key, count}`. A group of one is sent as a plain event. |
| Tenant limit | `object_type = Monitoring\Tenants`, `object = {tenant_id, resource, limit, count}`, `device` and `check` are `null`, `subject = tenant:<id>` |
| `webhook.test` | `subject` is the webhook ID. `object_type = Monitoring\\Webhooks`, `data.object` is the webhook (no secret), `data.actor` is the caller. |

## 9. Alert routes: what decides who is notified

A route has `match`, an `endpoint_id`, and optional grouping, repeat and steps. Routes are evaluated in order. The first match wins unless `continue` is true.

| Setting | Values | Effect |
| --- | --- | --- |
| `match.event_types` | any type above | Empty means the default set (opened, updated, acknowledged, resolved, tenant limits) |
| `match.severity` | `warning`, `critical` | |
| `match.device_types` | device types | |
| `match.site_ids` | site UUIDs | |
| `match.device_ids` | device UUIDs | |
| `match.check_ids` | check UUIDs | |
| `match.tags` | key and value map | Device must carry every tag |
| `group_by` | `root_device_id`, `device_id`, `site_id`, `severity`, `check_id`, `plugin`, `device_type` | Combines incidents into one event |
| `group_wait_seconds` | 0 to 600, default 30 | Wait before sending a group |
| `repeat_interval_seconds` | 300 to 604800 | Enables `renotify` |
| `steps[]` | `after_seconds`, `labels`, `only_if_unacknowledged`, `schedule` | Enables `escalated`. A schedule has a time zone, weekdays and from/to times. The first step cannot have one. |
| `continue` | bool | Keep evaluating later routes after a match |
| Limits | `max_webhooks` 20, `max_alert_routes` 50 per tenant | Create beyond the limit returns 409 `limit-reached` |

## 10. Delivery behavior customers should know

- At least once. Deduplicate on `webhook-id`.
- Ordered per incident and per endpoint. No global order.
- Retries: 5 s, 30 s, 2 min, 10 min, 30 min, then hourly, for 24 h, then `failed`. Failed deliveries can be replayed (single or bulk).
- A `410 Gone` answer disables the endpoint, with `disabled_reason`.
- Delivery log: `GET /v1/webhooks/{id}/deliveries`, kept 30 days by default.
- Webhook URLs on private or blocked networks are refused when saved (422) and again at delivery.
- `POST /v1/alert-routes/test` shows which routes a sample incident would match.
