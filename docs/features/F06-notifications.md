# F06: Webhook notifications and alert routes

**Status:** Draft · **Phase:** MVP (webhooks, routes, grouping, repeat); phase 2 (escalation steps, schedules) · **Related:** [ADR-0009](../adr/0009-webhook-delivery.md), [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)

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
- **Steps (phase 2):** each step is a separate event with its own labels; the receiver (n8n, Node-RED, PlusClouds) decides who the labels mean. Acknowledgement stops later steps. Schedules (phase 2) restrict a step to time ranges, for business hours versus night.
- In the MVP, a route has exactly one step at `0m`.

### Event contents

Events use the CloudEvents envelope shared with PlusClouds ([ADR-0009](../adr/0009-webhook-delivery.md)). Each event carries enough for the receiver to build a message without calling back, including the external IDs of the tenant, the device and its parents, and the acknowledging user, so PlusClouds can map the event to its own objects without a lookup: incident fields, device (name, address, type, tags, parent path), check (name, plugin, last output, thresholds), runbook URL, a link to the incident in the panel (URL template per tenant), and the route step labels. Grouped events carry their incidents as `data.objects`.

### Delivery

As in [ADR-0009](../adr/0009-webhook-delivery.md): outbox, Standard Webhooks signing, retries with backoff for 24 h, per-incident ordering, delivery log with replay.

## API

`/v1/webhooks` (CRUD, `by-external-id/{external_id}`, `{id}/test`, `{id}/deliveries`, `{id}/deliveries/{d}/replay`, `{id}/rotate-secret`), `/v1/alert-routes` (CRUD, `by-external-id/{external_id}`, `test` with a sample incident to show which routes match), `/v1/schedules` (phase 2).

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

Steps and schedules stay in phase 2.

## Acceptance criteria

- An n8n flow using a Standard Webhooks verification step accepts every event.
- 50 devices behind one switch going down produce one grouped event for the switch (with dependency suppression, phase 2) or one grouped event per `group_by` value (MVP).
- A receiver that is down for 1 hour receives every event after it recovers, in order per incident.
- Killing the notifier mid-delivery never loses an event; duplicates carry the same `webhook-id`.

## Open questions

- Exact fields of `data.object` for incidents are fixed with the OpenAPI incident schema; the envelope around it is decided ([ADR-0009](../adr/0009-webhook-delivery.md)).
