# F13: Usage metering

**Status:** Draft · **Phase:** MVP (metering and usage API); phase 3 (usage reports from customer-run servers) · **Related:** [F01](F01-tenancy-auth-audit.md), [F03](F03-plugin-sdk.md), [F04](F04-scheduler-and-runner.md), [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)

## Summary

Customers are billed per check on each device: a device with a ping check and an HTTP check is two billable units. The engine measures how long each check was active and exposes that as usage records. PlusClouds turns usage into prices and invoices; the engine never knows prices.

Usage is per tenant (a PlusClouds account). Users are not billed; the audit log still shows who created each check.

## Billing unit: the check-hour

A **check-hour** is one check, enabled on a device, for one hour (or part of it).

- Metered per check, with the device, plugin type and billing class attached, so an invoice can list "sw-core-1: icmp, snmp.system, snmp.interfaces".
- The price per check-hour depends on the plugin's **billing class**, not on each plugin, so adding a plugin never needs a price change in PlusClouds.
- The interval is recorded but does not change the unit. If faster intervals should cost more, PlusClouds prices by the recorded interval band (see open questions).

### Billing classes

Declared in the plugin manifest ([F03](F03-plugin-sdk.md)) as `BillingClass`:

| Class | Plugins (MVP) | Why |
| --- | --- | --- |
| `basic` | `icmp`, `tcp`, `udp`, `dns`, `tls`, `whois` | One cheap request per run |
| `standard` | `http`, `snmp.system`, `snmp.get`, `snmp.ups`, `snmp.pdu`, `snmp.sensor`, `push.http`, `push.mqtt` | More work per run or per message |
| `advanced` | `snmp.interfaces`, `redfish.health`, `ipmi.sensors`, `rtsp.stream`, `xapi.pool`, `xapi.rrd` | Collectors, table walks, stream reads, BMC and hypervisor sessions |
| `free` | `mqtt.connection` | Added automatically as a companion of a push check; not billed separately |

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
| Auto-registered sensor (F12) | Yes, from registration; messages dropped over the device limit are not billed |
| Checks of the platform tenant (self-monitoring) | No |

Collectors are one check each, whatever they discover. A collector that finds 200 VMs is one `advanced` check-hour per hour, and the VMs it creates are not billed separately (see open questions).

## Behavior

### Recording active time

Sampling cannot see a check that exists for ten minutes between two samples, so active time is recorded from events instead:

- `check_billing_periods (id, tenant_id, check_id, device_id, plugin, billing_class, interval_seconds, runs_on, started_at, ended_at)`.
- A period **starts** when a check is created enabled, enabled, or its tenant is reactivated. It **ends** when the check is disabled or deleted, its device is deleted, or its tenant is suspended or deleted.
- A change of a recorded attribute (interval, probe) ends the current period and starts a new one, so each period has one set of attributes.
- Periods are written in the **same transaction** as the change that causes them, like audit events. A check can never be active without an open period.
- A consistency job (hourly, in the `maintenance` role) compares enabled checks with open periods and repairs and reports any mismatch as a self-monitoring alert ([F11](F11-self-monitoring.md)).

This works the same whether a check came from the API, a template, a tenant config file or auto-registration, because all of them end in the same database writes.

### Daily usage records

The `maintenance` role closes each UTC day one hour after midnight and writes one row per check per day:

`usage_check_days (tenant_id, day, check_id, device_id, device_name, plugin, billing_class, interval_seconds, runs_on, active_seconds, check_hours, executions)`

- `check_hours` = active hours, each started hour counted once (a check active 10:15–10:40 is 1 check-hour).
- `executions` comes from the runner's counters and is informational only; it is not the billing unit.
- `device_name` is copied at close so an invoice stays readable after a device is renamed or deleted.
- A closed day is **never rewritten**. Late corrections are written as separate adjustment rows (`kind: adjustment`, with a reason), so an invoice that was already issued can always be reproduced.

A second table holds totals per tenant, day and billing class for fast invoice summaries.

### Retention

Usage tables are not part of metric retention. They are kept for a platform-level period (`platform.usage_retention`, default proposal 13 months) and partitioned by month so old months are dropped cheaply.

## API

| Endpoint | Caller | Returns |
| --- | --- | --- |
| `GET /v1/usage/summary?from=&to=&group_by=day,billing_class,plugin` | tenant (any role), platform | Check-hours per group |
| `GET /v1/usage/checks?from=&to=&device_id=&plugin=` | tenant (any role), platform | Per-check daily lines, cursor-paginated |
| `GET /v1/usage/tenants?from=&to=` | platform only | Summary per tenant, with tenant external IDs, for billing runs |
| `GET /v1/usage/current` | tenant, platform | Checks currently active by billing class (running estimate, not billable) |

- `from` and `to` are whole UTC days. Responses mark each day `closed: true|false`; billing uses closed days only.
- leo4 runs its billing job by pulling closed days. Pulling the same period twice returns the same data, so a failed job is simply rerun.

## Customer-run servers (phase 3)

A server that is not hosted by PlusClouds can only be billed if it reports. A server connected to PlusClouds sends each closed day's usage to leo4, signed with its installation key and carrying an `installation_id`. Standalone servers that do not connect are not metered; the MIT license gives no way to enforce it.

## Acceptance criteria

- A check created at 10:15 and deleted at 10:40 produces 1 check-hour for that day.
- A device with `icmp` and `http` checks, enabled all day, produces 48 check-hours: 24 `basic` and 24 `standard`.
- Disabling a check stops its check-hours from the next hour; re-enabling starts a new period.
- Suspending a tenant ends every open period in the same transaction.
- Rerunning the day close for a closed day changes nothing.
- A test walks every code path that creates, enables, disables or deletes a check (API, bulk, template apply, config apply, auto-registration, device delete, tenant suspend/delete) and asserts the matching period change.
- For 10,000 devices with 5 checks each, closing a day takes under 60 s.

## Open questions

- **Collectors:** bill a hypervisor or switch collector as one check, or per discovered child (per VM, per interface)? Per child tracks value better but makes a price depend on what the customer's infrastructure contains.
- **Interval bands:** should a 5-second ping cost more than a 5-minute ping? If yes, PlusClouds prices by `interval_seconds` bands; the engine already records it.
- **Minimum unit:** hourly as proposed, or daily (a check active for any part of a day is billed for the day)?
- **Storage:** bill metric storage (series, bytes per retention class) separately, or include it in the check price?
- **Billing class assignments:** confirm the table above with PlusClouds pricing.
