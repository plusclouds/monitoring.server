# F06: Webhook notifications and alert routes

**Status:** Implemented (webhooks, routes, grouping, repeat, escalation steps, schedules) · **Related:** [ADR-0009](../adr/0009-webhook-delivery.md), [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)

## Summary

Incident changes become events. Routes decide which webhook endpoint receives which events. The notifier delivers them signed, retries failures and records every attempt.

## Behavior

### Webhook endpoints

- Fields: `name`, `url`, `enabled`, `secret` (generated, shown once), `headers` (static extra headers, encrypted), `timeout`, `external_source` / `external_type` / `external_id` (the PlusClouds notification channel or integration this endpoint belongs to).
- Webhook endpoints are stored in the engine. When a PlusClouds account enables monitoring, leo4 upserts one endpoint pointing at its event receiver by external ID ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md#plusclouds-receiver)), which re-fires events into PlusClouds' own listeners and pushers; tenants can add their own (n8n, Node-RED) next to it.
- `POST /webhooks/{id}/test` sends a signed `monitoring.webhook.test` event immediately and returns the receiver's status and body (first 1 KB).

### Alert routes

A route matches incidents and sends them to an endpoint:

```json
{
  "name": "datacenter critical",
  "match": { "severity": ["critical"], "site_ids": ["…"], "tags": { "rack": "a12" }, "device_types": ["server", "network"] },
  "endpoint_id": "…",
  "group_by": ["root_device_id"],
  "group_wait": "30s",
  "repeat_interval": "4h",
  "continue": false,
  "steps": [
    { "after": "0m",  "labels": { "recipient": "noc" } },
    { "after": "15m", "labels": { "recipient": "on-call" }, "only_if_unacknowledged": true }
  ]
}
```

- Routes are evaluated in order; the first match wins unless `continue` is true.
- **Grouping:** incidents with the same `group_by` values within `group_wait` are sent as one event with a list of incidents. This keeps a switch failure to one message.
- **Repeat:** open, unacknowledged incidents are re-sent as `monitoring.incident.renotify` every `repeat_interval`.
- **Steps:**
  - Each step is a separate event with its own labels; the receiver (n8n, Node-RED, PlusClouds) decides what the labels mean.
  - Acknowledging an incident stops the later steps that set `only_if_unacknowledged`.
  - A schedule restricts a step to time windows, such as business hours versus night.

### Event contents

Events use the CloudEvents envelope shared with PlusClouds ([ADR-0009](../adr/0009-webhook-delivery.md)). Each event carries enough for the receiver to build a message without calling back, including the external IDs of the tenant, the device and its parents, and the acknowledging user, so PlusClouds can map the event to its own objects without a lookup: incident fields, device (name, address, type, tags, parent path), check (name, plugin, last output, thresholds), runbook URL, a link to the incident in the panel (URL template per tenant), and the route step labels. Grouped events carry their incidents as `data.objects`.

### Delivery

As in [ADR-0009](../adr/0009-webhook-delivery.md): outbox, Standard Webhooks signing, retries with backoff for 24 h, per-incident ordering, delivery log with replay.

## API

`/v1/webhooks` (CRUD, `by-external-id/{external_id}`, `{id}/test`, `{id}/deliveries`, `{id}/deliveries/{d}/replay`, `{id}/deliveries/replay` in bulk, `{id}/rotate-secret`), `/v1/alert-routes` (CRUD, `by-external-id/{external_id}`, `test` with a sample incident to show which routes match). Schedules are part of each step rather than a separate resource.

## Implementation status

M2 implements endpoints, rotation, ordered routes with `continue`, signed delivery with retries, ordering and the delivery log. M4 adds grouping and repeats:

- **Grouping.**
  - `group_by` can use `root_device_id`, `device_id`, `site_id`, `severity`, `check_id`, `plugin` and `device_type`. `group_wait_seconds` runs from 0 to 600 and defaults to 30.
  - Groups are kept per route, event type and key.
  - A group of one is sent as the plain event. A larger group is sent as one event of the same type, with `data.objects` (each incident's `object`, `device` and `check`) and `data.group` (`id`, `key`, `count`); `data.object`, `data.device` and `data.check` are null.
- **Repeats.**
  - `repeat_interval_seconds` runs from 300 to 604800.
  - Open, unacknowledged, unsuppressed incidents are re-sent to that route only as `monitoring.incident.renotify`.
  - Acknowledging or resolving an incident stops its repeats.
- **Suppressed incidents** ([F05](F05-state-and-incidents.md)) are not notified.

Completed after M4 part 1:

- **Escalation steps.** A route's `steps` list starts with the notification itself.
  - **Timing.** Each later step is sent `after_seconds` after the notification, as `monitoring.incident.escalated`, to that route only.
  - **Content.** The route's labels are merged with the step's labels, and the step number goes in `data.route.step`.
  - **Skips.** A step is skipped when the incident has been resolved or suppressed, or acknowledged if the step sets `only_if_unacknowledged`. Every outcome is recorded in `route_escalations`.
- **Schedules.** A step can carry a schedule with a time zone, weekdays and from/to times. A window whose `to` is before its `from` spans midnight. A step that falls due outside its windows waits for the next one. The first step cannot have a schedule.
- **Route test.** `POST /v1/alert-routes/test` builds the event of a sample incident from a real device and check, and lists for every route whether it matches and whether it would deliver.
- **Bulk replay.** `POST /v1/webhooks/{id}/deliveries/replay?status=failed&from=&to=` queues an endpoint's failed or cancelled deliveries again, up to 10,000 per call.
- **Payload additions.**
  - `data.links.incident` from `notifier.incident_url_template` (placeholders `{incident_id}`, `{account_id}` and `{tenant_id}`).
  - `data.check.thresholds`.
- **Incident list.** `GET /v1/incidents?suppressed=` filters on suppression.

- **Limits.** Tenant limits `max_webhooks` (default 20) and `max_alert_routes` (default 50) can be set with the platform tenant PATCH. A create beyond either limit returns 409 `limit-reached`.
- **URL check when saved.** A webhook URL that the tenant's network policy blocks is refused with 422 `invalid-value` when it is created or updated. The check covers IP literals, `localhost`, and the addresses a hostname resolves to at that moment, and it is the same check that runs at delivery. Delivery still checks again, because DNS can change.
- **Delivery retention.** `notifier.delivery_retention` (default 30 days) deletes finished deliveries, then routed events that no delivery or open group still needs. Pending deliveries are never deleted.

- **Limit events.** `monitoring.tenant.device_limit_reached` and `monitoring.tenant.check_limit_reached` are written when a create brings the account to its limit (the next create is refused with 409 `limit-reached`). `data.object_type` is `Monitoring\Tenants`, `data.object` is `{tenant_id, resource, limit, count}`, `data.device` and `data.check` are null, and the subject is `tenant:<id>`. Routes deliver them by default; a route that filters on device, check or severity does not, and they are never grouped, repeated or escalated. Lowering a limit below the current count sends nothing.

Still not built: `monitoring.heartbeat` (planned with F11 in M5).

## Acceptance criteria

- An n8n flow using a Standard Webhooks verification step accepts every event.
- 50 devices behind one switch going down produce one grouped event for the switch (with dependency suppression, phase 2) or one grouped event per `group_by` value (MVP).
- A receiver that is down for 1 hour receives every event after it recovers, in order per incident.
- Killing the notifier mid-delivery never loses an event; duplicates carry the same `webhook-id`.

## Open questions

- Exact fields of `data.object` for incidents are fixed with the OpenAPI incident schema; the envelope around it is decided ([ADR-0009](../adr/0009-webhook-delivery.md)).
