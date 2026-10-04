# F13: Usage metering

**Status:** Draft · **Phase:** MVP (metering and usage API); phase 3 (usage reports from customer-run servers) · **Related:** [F01](F01-tenancy-auth-audit.md), [F03](F03-plugin-sdk.md), [F04](F04-scheduler-and-runner.md), [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)

## Summary

Customers are billed for checks only. Each check counts with the **weight** of its plugin type: a device with a ping check (weight 1) and an HTTP check (weight 2) is 3 weighted units per hour. The engine measures how long each check was active and returns usage per device as check-hours times weights. PlusClouds turns weighted units into prices and invoices; the engine never knows prices.

Usage is per tenant (a PlusClouds account). Users are not billed; the audit log still shows who created each check.

## Billing unit: the check-hour

A **check-hour** is one check, enabled on a device, for one hour (or part of it).

- Metered per check, with the device and plugin type attached, so an invoice can list "sw-core-1: icmp, snmp.system, snmp.interfaces".
- The interval is recorded but does not change the unit.

### Weights (decided 2026-10-04)

Only checks are billed, and each counts by its plugin's weight. There are no tiers or classes.

- Every plugin declares a default weight in its manifest (`BillingWeight`, replacing `BillingClass`), for example `icmp` 1, `http` 2, `snmp.interfaces` 5. Defaults are confirmed with PlusClouds pricing before F13 ships.
- The platform overrides a plugin's weight with `PUT /v1/usage/weights/{plugin}`; every check of that plugin, in every tenant, uses it. There is no per-check or per-tenant weight.
- A weight change applies from the hour it is made. The weight in force is recorded on every usage line, so an invoice for a past period is reproduced exactly.
- Companion checks that exist only to support another check (`mqtt.connection` next to a push check) have weight 0.
- **Usage returned:** per device, `weighted_units = Σ over its checks (check_hours × weight)`, with the per-check lines (check-hours, weight, units) underneath for the invoice.

### What counts

| Case | Billed |
| --- | --- |
| Check enabled, device and tenant active | Yes |
| Check disabled | No, from the moment it is disabled |
| Device or check inside a maintenance window | Yes; the check keeps running |
| Check run by a remote probe instead of the core | Yes, same unit; `runs_on` records where |
| `run-now`, device test, credential test | No |
| Tenant suspended | No check-hours (checks stop); storage is out of scope for this unit |
| Tenant soft-deleted | No, from the moment of deletion |
| Auto-registered sensor (F12) or LLM application (F14) | Yes, as its push check, from registration. Messages dropped over the device limit are not billed |
| Checks of the platform tenant (self-monitoring) | No |

### Discovered devices are not billed

Devices a collector creates (VMs, pool hosts) are not billed on their own; only checks are. A collector counts as one check with its plugin's weight, so its weight should reflect how much it watches. A check a user adds on a discovered device (a ping on a VM) is billed like any other check.

### Example: a cloud provider with 5 hypervisors, iDRAC and 50 VMs

With example weights `icmp` 1, `redfish.health` 3, `xapi.rrd` 5, `xapi.pool` 5, `http` 2:

| Device | Count | Checks | Weighted units per hour |
| --- | --- | --- | --- |
| iDRAC | 5 | `icmp`, `redfish.health` | 5 × (1 + 3) = 20 |
| Hypervisor host | 5 | `icmp`, `xapi.rrd` | 5 × (1 + 5) = 30 |
| Pool | 1 | `xapi.pool` | 5 |
| VM | 50 | none; metrics come from `xapi.rrd` | 0 |

Monthly (730 hours): 40,150 weighted units. Adding an HTTP check to 10 of the VMs adds 10 × 2 × 730 = 14,600.

### Packages

The engine meters only weighted check units. PlusClouds may sell packages on top ("physical server", "per VM") by mapping a package to units, so customers see a simple offer and the invoice stays traceable to the meter.

## Behavior

### Recording active time

Sampling cannot see a check that exists for ten minutes between two samples, so active time is recorded from events instead:

- `check_billing_periods (id, tenant_id, check_id, device_id, plugin, weight, interval_seconds, runs_on, started_at, ended_at)`.
- A period **starts** when a check is created enabled, enabled, or its tenant is reactivated. It **ends** when the check is disabled or deleted, its device is deleted, or its tenant is suspended or deleted.
- A change of a recorded attribute (interval, probe, the plugin's weight) ends the current period and starts a new one, so each period has one set of attributes.
- Periods are written in the **same transaction** as the change that causes them, like audit events. A check can never be active without an open period.
- A consistency job (hourly, in the `maintenance` role) compares enabled checks with open periods and repairs and reports any mismatch as a self-monitoring alert ([F11](F11-self-monitoring.md)).

This works the same whether a check came from the API, a template, a tenant config file or auto-registration, because all of them end in the same database writes.

### Daily usage records

The `maintenance` role closes each UTC day one hour after midnight and writes one row per check per day:

`usage_check_days (tenant_id, day, check_id, device_id, device_name, plugin, weight, interval_seconds, runs_on, active_seconds, check_hours, weighted_units, executions)`

- `check_hours` = active hours, each started hour counted once (a check active 10:15–10:40 is 1 check-hour). `weighted_units` = `check_hours × weight`.
- `executions` comes from the runner's counters and is informational only; it is not the billing unit.
- `device_name` is copied at close so an invoice stays readable after a device is renamed or deleted.
- A closed day is **never rewritten**. Late corrections are written as separate adjustment rows (`kind: adjustment`, with a reason), so an invoice that was already issued can always be reproduced.

A second table holds weighted units per tenant, device and day for fast invoice summaries.

### Retention

Usage tables are not part of metric retention. They are kept for a platform-level period (`platform.usage_retention`, default proposal 13 months) and partitioned by month so old months are dropped cheaply.

## API

| Endpoint | Caller | Returns |
| --- | --- | --- |
| `GET /v1/usage/devices?from=&to=` | tenant (any role), platform | Per device: `weighted_units`, with its checks' lines (plugin, check-hours, weight, units), per day |
| `GET /v1/usage/tenants?from=&to=` | platform only | Weighted units per tenant, with tenant external IDs, for billing runs |
| `GET /v1/usage/current` | tenant, platform | Weighted units per hour of the checks active now (running estimate, not billable) |
| `GET /v1/usage/weights` | any | Weight per plugin |
| `PUT /v1/usage/weights/{plugin}` | platform only | Set a plugin's weight from the current hour; audited |

- `from` and `to` are whole UTC days. Responses mark each day `closed: true|false`; billing uses closed days only.
- leo4 runs its billing job by pulling closed days. Pulling the same period twice returns the same data, so a failed job is simply rerun.

## Customer-run servers (phase 3)

A server that is not hosted by PlusClouds can only be billed if it reports. A server connected to PlusClouds sends each closed day's usage to leo4, signed with its installation key and carrying an `installation_id`. Standalone servers that do not connect are not metered; the MIT license gives no way to enforce it.

## Acceptance criteria

- A check created at 10:15 and deleted at 10:40 produces 1 check-hour for that day.
- A device with `icmp` (weight 1) and `http` (weight 2) checks, enabled all day, produces 48 check-hours and 72 weighted units.
- An XCP-ng pool whose collector discovers 50 VMs is billed as its collector check only.
- Changing a plugin's weight at 14:20 bills that day's checks of the plugin with the old weight until 14:00 and the new one from 14:00 on; reading an earlier closed day returns the old weight.
- Disabling a check stops its check-hours from the next hour; re-enabling starts a new period.
- Suspending a tenant ends every open period in the same transaction.
- Rerunning the day close for a closed day changes nothing.
- A test walks every code path that creates, enables, disables or deletes a check (API, bulk, template apply, config apply, auto-registration, device delete, tenant suspend/delete) and asserts the matching period change.
- For 10,000 devices with 5 checks each, closing a day takes under 60 s.

## Open questions

- **What billing needs from the API:** PlusClouds' billing team is confirming how usage reaches billing (pull of closed days as above, or a push).
- **LLM units** ([F14](F14-llm-monitoring.md)): confirm spans per 1,000, content GB-days and judge evaluations as separate units.

## Decided

- **Checks only, weighted** (2026-10-04): no tiers or classes and no discovered-object unit. Each plugin has a platform-set weight; usage per device is check-hours × weights.
- **Auto-registered devices** (MQTT sensors, LLM applications) are billed through their own push check.

- **Unit length:** hourly. A check active for any part of an hour is billed for that hour.
- **Interval:** no surcharge for short intervals at first. `interval_seconds` is recorded on every line, so interval bands can be priced later without engine changes.
- **Storage:** included in the check price. Revisit when usage data shows storage per tenant varying widely.
- **Collectors:** one check each, with their plugin's weight; discovered devices are not billed.
