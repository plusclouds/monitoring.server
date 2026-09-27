# F05: State machine and incidents

**Status:** Draft · **Phase:** MVP (state machine, thresholds, hysteresis, flap detection, incidents); phase 2 (dependency suppression, maintenance windows) · **Related:** design section 6

## Summary

The engine turns results into state, and state changes into incidents. It is the only place where thresholds are evaluated and where noise control happens, so every plugin gets the same behavior.

## Behavior

### Evaluating a result

1. **Plugin status**, from the result (protocol facts: unreachable, HTTP 500, PSU failed).
2. **Threshold status**, from the check's threshold rules applied to the result's metrics.
3. **Effective status** = the worse of the two. UNKNOWN (plugin error, timeout of the monitoring itself) is kept separate from CRITICAL: it opens an incident only if the check's `unknown_is_critical` flag is set (default off), so a broken probe does not page people as if devices were down.

### Threshold rules

```json
{
  "metric": "cpu_usage_percent",
  "warning":  { "op": ">", "value": 80 },
  "critical": { "op": ">", "value": 90 },
  "for": "5m",
  "hysteresis": 5
}
```

- Operators: `>`, `>=`, `<`, `<=`, `==`, `!=`, `between` and `outside` (with `value` and `value_max`). Unknown operators are rejected when the rule is saved.
- A metric may have several rules; each has an `id` and optional `name`, and the worst matching level wins. The matched rule is recorded on the incident.
- `for`: the condition must hold continuously for this long before the status changes (for metrics that are noisy per sample).
- `hysteresis`: to go back to a better status, the value must cross the threshold by this margin (alert above 90, clear below 85).
- Rules can match per-object metrics from collectors (`object: "*"` for every interface or VM, or a label filter).

### State machine

One `check_state` row per check (and per object for collectors with per-object thresholds):

| From | Event | To | Side effect |
| --- | --- | --- | --- |
| OK | bad result | PENDING | none |
| PENDING | good result | OK | blip counted, not alerted |
| PENDING | `failure_count` bad results in a row (default 3), or `for` elapsed | PROBLEM | incident opened |
| PROBLEM | good results past hysteresis, `recovery_count` in a row (default 1) | OK | incident resolved |
| PROBLEM | severity changes (WARNING ↔ CRITICAL) | PROBLEM | incident updated, event sent |

Acknowledgement belongs to the incident, not the state: an acknowledged incident stays open until recovery.

### Flap detection

- Keep the last 21 status transitions per check. If more than 30 % of the last 20 intervals had a transition, mark the check **flapping**: send one `incident.opened` (or update) with `flapping: true`, then suppress further open/resolve events until the rate drops below 20 %.

### Incidents

- One incident per check (per object for collectors) per problem episode. Fields: `severity`, `status` (`open`, `acknowledged`, `resolved`), `opened_at`, `acknowledged_at/by`, `resolved_at`, `root_device_id`, `suppressed`, `flapping`, `summary`, `last_output`, `comments`.
- Incident writes and their outbox events are one transaction ([ADR-0009](../adr/0009-webhook-delivery.md)).

### Dependency suppression (phase 2, data model in MVP)

- When a device's **host check** (flagged `is_host_check`, typically ping) is in PROBLEM, incidents on its descendants are opened as `suppressed` and linked to the root incident, and no notifications are sent for them.
- Order matters: when a switch dies, its children's checks may fail before the switch's own check. Child incidents wait `dependency_grace` (default 30 s) before notifying, and are suppressed if an ancestor opens an incident within that window.
- When the root recovers, suppressed children that are still failing are re-evaluated and notify normally.

### Maintenance windows (phase 2)

- Scope by device IDs, tags or a subtree; always with an end time. Checks keep running and state keeps changing; only notifications are withheld. Incidents still open when the window ends notify at that moment.

### Late and stale results

- Results older than 2× the check interval (buffered probe data) update metrics but not state.
- If no result arrives for 3× the interval, the state goes to UNKNOWN with output "no result" (catches stuck runners and dead probes).

### Concurrency

In the MVP, state for a check is processed only by the node that owns its shard. Probe results arrive at whichever core node the probe is connected to; state rows carry a version number and updates use optimistic concurrency, retrying on conflict.

## API

`/v1/checks/{id}/state`, `/v1/incidents` (list with filters: status, severity, device, tag, suppressed), `/v1/incidents/{id}` with `ack`, `resolve` (manual resolve is allowed; it reopens on the next bad result), `comment`. State changes are published on the SSE stream.

## Acceptance criteria

- A single failed ping produces no notification; three in a row produce exactly one `incident.opened`.
- A value oscillating between 88 and 92 with threshold 90 and hysteresis 5 produces one incident, not many.
- A check toggling every interval produces one flapping notification in 20 intervals.
- Engine processing at 1,000 results/s adds under 50 ms p99 latency between result and state write.

## Open questions

- Should incidents group per device (one incident "server X has 4 problems") instead of per check? Proposed: incidents stay per check; grouping happens in notifications ([F06](F06-notifications.md)).
