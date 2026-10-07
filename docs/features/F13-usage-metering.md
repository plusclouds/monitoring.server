# F13: Usage metering

**Status:** Implemented in M3.5; counting rules and weights confirmed by PlusClouds on 2026-10-07 · **Phase:** MVP (metering and usage API); phase 3 (usage from customer-run servers) · **Related:** [F01](F01-tenancy-auth-audit.md), [F03](F03-plugin-sdk.md), [F04](F04-scheduler-and-runner.md), [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)

## Summary

Customers are billed for checks only. For every closed UTC hour and every tenant, the engine measures how many seconds each check was billable. It weights those seconds by the check's plugin and serves the result through a usage API that PlusClouds billing pulls. PlusClouds applies prices and free tiers. The engine never knows prices, and billing never needs to know the weights.

Usage is per tenant: one PlusClouds account, identified by the tenant's external ID (the `iam_accounts` UUID). Users are not billed; the audit log still shows who created each check.

## Meters

| Field | Unit | Billed | Meaning |
| --- | --- | --- | --- |
| `billable_check_seconds` (meter `monitoring.check_second`) | weighted check-seconds | Yes | Σ over the tenant's billable checks of (seconds billable in the hour × the plugin's weight) |
| `device_seconds` (meter `monitoring.device_second`) | device-seconds | No, informational | Σ over the tenant's devices of seconds they existed in the hour |

Each hour's figure comes with the breakdown that produced it: per plugin, the unweighted check-seconds and the weight. For example, 2 `icmp` checks (weight 1) and 2 `http` checks (weight 2) over a full hour give 2 × 3,600 × 1 + 2 × 3,600 × 2 = 21,600, which is 6 weighted checks.

Seconds are time-weighted inside the hour: a check added at 10:15 counts 2,700 seconds in the 10:00 hour. The interval is recorded per period but does not change the meter.

### Weights

- **Where they are set.** The platform operator sets weights in the server config file, per plugin type. Every check of that plugin, in every tenant, uses that weight. There is no weight per check or per tenant, and no API to change weights:

  ```yaml
  usage:
    weights:            # plugin type: weight
      icmp: 1
      http: 2
    default_weight: 1   # plugins not listed
  ```

- **Weights confirmed by PlusClouds (2026-10-07):** `icmp` 1, `http` 2, `snmp.get` 1, `snmp.system` 1, `snmp.ups` 1, `snmp.sensor` 1, `snmp.pdu` 2, `snmp.interfaces` 3, `redfish.health` 2, `xapi.pool` 0 with `xapi.pool:host` 3 (each pool host, decided 2026-10-07), `camera.snapshot` 2, `rtsp.stream` 2; Whoopsy! at 5× the plugin's weight. Push and LLM checks get their weights when those plugins exist (M5, F14); until then `default_weight` 1 covers any other plugin, including `push.http` (from `v0.9.0`), which awaits a PlusClouds weight. PlusClouds reviews the weights after about a month of real usage. They are in `deploy/config.example.yaml`; they apply from the next full UTC hour after the operator deploys the config.
- **Applying a change.** At start, the server compares the file's weights with the weights in force in the database.
  - Each difference takes effect at the **next full hour** and is recorded with an audit event (actor `file`).
  - A change needs a restart, like the rest of the config; no release is needed.
  - Closed hours keep the weight they were computed with, so a weight change never revises a past hour.
- **History.** The database keeps every weight with the time it took effect. Every usage row carries the weights it was computed with.
- **Which node.** The maintenance node (one at a time) records the weights from its own config at start and every hour. Give every node the same `usage` section.
- **Whoopsy!** (2026-10-07): a check with Whoopsy! on is billed as `<plugin>+whoopsy` at the plugin's weight times `usage.whoopsy_multiplier` (default 5), unless `usage.weights` names `<plugin>+whoopsy`. The multiplier is recorded with the weights (`*whoopsy`) and follows the same history rules. See [F05](F05-state-and-incidents.md#whoopsy-premium-alerting-decided-2026-10-07).
- **Companion checks** exist only to support another check, such as `mqtt.connection` next to a push check. The plugin manifest marks them (`Billable: false`, replacing `BillingClass`), and they are never billed, whatever the config says.

### Billable collector objects (v0.8.0)

Rule 11 (collector objects are not billed separately) has one exception, decided by PlusClouds on 2026-10-07: the hypervisor hosts of an XCP-ng pool. A pool of 20 hosts produces far more work than one of 3, so the host, not the pool, is the unit.

- A plugin declares billable object kinds by key prefix (`xapi.pool`: `host:` objects as `xapi.pool:host`). Its weight comes from `usage.weights` like a plugin's; the pool check itself has weight 0.
- `object_periods` (like `check_periods`) runs from when the engine first sees the object until it is gone, the check is disabled or deleted, or the tenant is suspended or deleted; the same database functions as for checks keep them, so the same time rules apply. Disabling a check forgets its objects; their periods resume with the next collection.
- The hourly close adds them to the breakdown under their key (`"xapi.pool:host": {count_seconds, weight}`) and to `billable_check_seconds`. `GET /v1/usage/current` and `/v1/usage/weights` show them too. Counting starts at the deploy of v0.8.0; earlier hours are not back-billed. Migration `00018`.

### What counts

These rules were proposed by PlusClouds and confirmed by PlusClouds' billing on 2026-10-07, all fourteen as listed (the numbered list sent for confirmation matches the rows below, plus: collector objects are not billed separately; `device_seconds` is informational; no surcharge for short intervals, to be revisited if heavy users appear). A free tier is applied by PlusClouds in weighted check-seconds on its side.

| Case | Billed |
| --- | --- |
| Check enabled, device and tenant active | Yes, also while it fails or is UNKNOWN: a check that runs and reports a problem is working |
| Check disabled | No, from the second it is disabled |
| Check deleted, or its device deleted | Yes, until the second of deletion |
| Device or check in a maintenance window | Yes; the check keeps running |
| Check run by a remote probe | Yes, with the same meter; `runs_on` is recorded |
| `run-now`, device test, credential test | No |
| Tenant suspended | No, from the second of suspension until resume |
| Tenant soft-deleted | No, from the second of deletion (and again from restore) |
| Auto-registered sensor (F12) or LLM application (F14) | Yes, as its push check, from registration |
| Devices a collector discovers (VMs, pool hosts) | No. The collector is one check with its plugin's weight; a check a user adds on a discovered device is billed like any other |
| Platform tenant (self-monitoring) | No |

`device_seconds` counts every device of an active tenant while it exists.

## Behavior

### Recording billable time

Sampling cannot see a check that exists for ten minutes between two samples, so billable time is recorded from changes:

- **Tables.** Checks: `check_billing_periods (id, tenant_id, check_id, device_id, plugin, interval_seconds, runs_on, started_at, ended_at)`. Devices: `device_periods` in the same form.
- **Starting a period.** A period starts when a check is created enabled or is enabled, or when its tenant is resumed or restored.
- **Ending a period.** A period ends when the check is disabled or deleted, when its device is deleted, or when its tenant is suspended or deleted.
- **Changed attributes.** A change of a recorded attribute (interval, probe) ends the current period and starts a new one.
- **Same transaction.** Periods are written in the same transaction as the change that causes them, like audit events, so a check can never be billable without an open period.
- **Consistency job.** An hourly job in the `maintenance` role compares enabled checks with open periods. It repairs any mismatch and reports it as a self-monitoring alert ([F11](F11-self-monitoring.md)).

Periods are written the same way whether the check came from the API, a template, a tenant config file or auto-registration, because all of them end in the same database writes.

### Closing hours

The `maintenance` role closes each UTC hour 5 minutes after it ends. It writes:

- `usage_hours (tenant_id, period_start, revision, billable_check_seconds, device_seconds, closed_at, reason)`: one row per tenant and hour.
- `usage_hour_plugins (tenant_id, period_start, revision, plugin, count_seconds, weight)`: the breakdown of that row.

How closed hours behave:

- **Which tenants get a row.** A tenant gets a row when it had at least one device or check during the hour. Rows with zero billable seconds are kept in the table but left out of the API.
- **Closed hours never change.** A correction, which is rare and manual, writes the same hour again with `revision + 1` and a `reason`. Results arriving late cannot change usage, because usage depends on configuration, not on results.
- **Closing twice changes nothing.** After downtime, the job closes every missed hour in order before the API reports them as closed.

### Retention

Usage tables are not part of metric retention. They are kept for a platform-level period (`platform.usage_retention`, proposed 13 months) and partitioned by month.

## API

### `GET /v1/usage/tenants?from=&to=` (platform key)

This is the contract billing depends on.

- **Range.** `from` and `to` are whole-hour UTC instants, with `from < to`, covering at most 31 days. Anything else returns 422.
- **Open hours.** A range reaching into an hour that is not closed yet (the current hour, or the last hour within its 5-minute close delay) returns 422 with `problem.type …/usage-hour-open` and the last closed hour in `detail`. Billing can then re-read up to that hour.
- **Past hours.** Any closed hour inside usage retention works, so a missed run is caught up by reading the range again.
- **Items.** One item per tenant per hour, ordered by `period_start` and then `account_id`. The list uses the usual `cursor` and `limit` (max 500).
- **Revisions.** Only the latest revision of each hour is returned. The same range returns the same numbers until a revision changes.
- **Zero usage.** Tenants without an external ID (the platform tenant) and hours with zero billable seconds are left out.

```json
{
  "items": [
    {
      "account_id": "7b7efcf0-06b4-43ad-91ad-2897042b6aa6",
      "period_start": "2026-10-04T12:00:00Z",
      "period_end": "2026-10-04T13:00:00Z",
      "billable_check_seconds": 21600,
      "device_seconds": 7200,
      "revision": 1,
      "breakdown": {
        "icmp": { "count_seconds": 7200, "weight": 1 },
        "http": { "count_seconds": 7200, "weight": 2 }
      }
    }
  ],
  "next_cursor": null
}
```

### Other endpoints

| Endpoint | Caller | Returns |
| --- | --- | --- |
| `GET /v1/usage/hours?from=&to=` | tenant (any role) | The tenant's own closed hours in the same item shape |
| `GET /v1/usage/current` | tenant, platform | Weighted checks billable right now (a running estimate, not billable) |
| `GET /v1/usage/weights` | any | Weight in force per plugin, and the default weight |

## No push emitter

PlusClouds billing confirmed pull (2026-10-04): it reads `GET /v1/usage/tenants` every hour and re-reads earlier hours to catch up. No push emitter is built.

## Customer-run servers (phase 3)

A server that PlusClouds does not host can be billed only if it reports. One connected to PlusClouds would expose the same usage API, or push its closed hours to leo4, signed with its installation key. Standalone servers that do not connect are not metered; the MIT license gives no way to enforce it.

## Acceptance criteria

- A check created at 10:15 and deleted at 10:40 gives 1,500 × its weight in the 10:00 hour.
- 2 `icmp` and 2 `http` checks enabled for the whole hour, with weights 1 and 2, give `billable_check_seconds` 21,600.
- Disabling a check at 10:20 bills it for 1,200 seconds of that hour; re-enabling it starts a new period.
- Suspending a tenant ends every open period in the same transaction; resuming starts them again.
- An XCP-ng pool whose collector discovers 50 VMs bills only its collector check; the VMs show in `device_seconds`.
- Changing a plugin's weight and restarting at 14:20 keeps the old weight through the 14:00 hour and uses the new one from 15:00. A restart without a change records nothing.
- **Open hours:** a request for the current hour returns 422.
- **Repeatable reads:** reading the same closed range twice returns identical items.
- **Catching up:** after 3 hours of downtime, the missed hours are closed in order and then appear in the API.
- **Period changes:** a test goes through every way a check can be created, enabled, disabled or deleted, and asserts the matching period change. The ways are the API, bulk changes, template apply, config apply, auto-registration, device delete, and tenant suspend, resume, restore and delete.
- Closing one hour for 10,000 devices with 5 checks each takes under 10 seconds.

## Decided

- **Checks only, weighted by plugin** (2026-10-04). There are no tiers, classes or discovered-object units. Weights come from the server config file. Billing receives weighted check-seconds plus the breakdown and never needs the weights.
- **Device seconds** are reported for information and are not billed.
- **Hourly, time-weighted seconds.**
- **Pull only** (confirmed by PlusClouds billing): the usage API is the contract; no push emitter.
- **Interval:** there is no surcharge for short intervals; `interval_seconds` is recorded per period.
- **Storage** is included in the check price.

## Open questions

- **Weights for push and LLM checks** (F08, F12, F14), once those plugins exist.
- **LLM units** ([F14](F14-llm-monitoring.md)): whether spans, content storage and judge evaluations get their own meters.

## Implementation status (M3.5)

What exists and what was left out is recorded in [progress](../progress.md#m35--usage-metering-f13). Differences from the text above:

- **Periods are kept by database triggers** on `checks`, `devices` and `tenants` (migration `00010`), not by application code. Any path that changes a check, device or tenant therefore updates its periods in the same transaction, so the hourly consistency job is not needed.
- **Counting starts at deploy.** The migration opens periods for what exists at that moment.
- **No `runs_on` yet.** Remote probes arrive in M5.
- **No billable flag yet.** The manifest's `Billable` flag waits for the first companion plugin; until then, a weight of 0 in the config does the same.
- **Corrections** are written with `monitor admin usage-recompute --from --to --reason`.
