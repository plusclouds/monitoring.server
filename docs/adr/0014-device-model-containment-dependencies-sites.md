# ADR-0014: Device model — containment, dependencies and sites

**Status:** Proposed · **Date:** 2026-09-28

## Context

The design uses one tree, `devices.parent_id`, for two different things: navigation ("which VMs run on this host") and alert suppression ("which devices go dark when this switch dies"). These trees are not the same:

- A VM is contained in its host and also depends on it: both trees agree.
- A server sits in a rack and is reached through switch port 12, which depends on the core switch and a UPS. It has one container but several things it depends on.
- A self-hosted LLM endpoint ([F14](../features/F14-llm-monitoring.md)) depends on the GPU servers that run it, which are not its parents in any sense.
- LLDP/CDP topology discovery (phase 2) produces a graph of links, not a tree.

"Site" also exists only as free text on probes and as a tag, although probes, failover groups, device location and ISO 3166 country codes ([compliance](../compliance/README.md#standards-the-engine-implements)) all need it.

The question also came up whether monitored items should be modeled as "assets" with probes under them.

## Options considered

1. **Keep one tree.** Simple, but suppression is wrong for any device with more than one upstream dependency, and topology discovery has nowhere to write.
2. **Containment tree plus a separate dependency graph, and sites as a table.** Two relations with one meaning each.
3. **A full CMDB-style graph with typed relations.** Flexible, but CMDB features are out of scope and every query becomes a graph query.

## Decision

Option 2.

### Entities

| Entity | Meaning |
| --- | --- |
| **Site** | A physical or logical location: datacenter, branch, customer premises. Fields: `name`, `country` (ISO 3166-1 alpha-2), `timezone`, `address` (free text), external IDs (PlusClouds `Datacenters`). Tenant-owned |
| **Device** | Anything with its own address or its own lifecycle: switch, BMC, server, hypervisor host, pool, VM, camera, UPS, sensor, website, LLM endpoint, LLM application. Has `site_id` (optional) |
| **Object** | A part of a device reported by a collector: interface, fan, PSU, disk, storage repository, GPU. Not a device: it has metrics, thresholds and incidents, but no own checks, credentials or billing |
| **Probe** | An agent that runs checks. Belongs to a site. Not part of the device tree: a device points to the probe that runs its checks (`probe_id`) |

We keep the name **device**, not asset. "Asset" implies CMDB data (owner, cost, warranty, contracts), which is out of scope. If PlusClouds shows these as assets, the external ID links them.

### Two relations

| Relation | Shape | Meaning | Set by |
| --- | --- | --- | --- |
| `devices.parent_id` | Tree | **Contains**: pool → host → VM; chassis → blade | Users, and collectors for what they discover |
| `device_dependencies (tenant_id, device_id, depends_on_id, source, created_at)` | Directed graph, no cycles | **Needs to work**: server → switch; switch → UPS; LLM endpoint → GPU server | Users; LLDP/CDP discovery in phase 2 (`source: lldp`); collectors (a VM depends on its host) |

Rules:

- Containment implies dependency: a child always depends on its parent, without a stored row. The graph holds only the extra edges.
- Cycles are rejected on write.
- **Suppression** ([F05](../features/F05-state-and-incidents.md)) walks the dependency graph (including implied containment edges) upward: an incident is suppressed if any device it depends on, directly or transitively, has its host check in PROBLEM. When several roots are down, the child links to the nearest one.
- A silent probe acts as a dependency of every device it runs ([F09](../features/F09-remote-probes.md)), also without a stored row.
- Navigation, the API's `children` endpoint, templates and billing ([F13](../features/F13-usage-metering.md)) use containment only.

### Where things live

```text
Tenant
├── Sites
│   └── Probes
├── Devices (containment tree via parent_id, each with an optional site)
│   ├── Checks and collectors
│   │   └── Objects (interfaces, fans, disks, GPUs)
│   └── Child devices (VMs, discovered by collectors)
├── Device dependencies (graph)
├── Credentials, templates
└── Routes, webhooks, maintenance windows
```

Grouping for routes, maintenance windows and template application stays tag and selector based. There is no folder hierarchy.

### API

`/v1/sites` (CRUD, by external ID), `site_id` on devices and probes with list filters, `/v1/devices/{id}/dependencies` (list, add, remove), `/v1/devices/{id}/dependents`, and `GET /v1/devices/{id}/impact` (every device that would be suppressed if this one fails).

## Consequences

- Suppression becomes correct for devices with several upstreams, and LLDP discovery has a place to write.
- The suppression walk is a recursive query instead of a parent walk. It runs only when an incident opens, and the graph is cached per tenant in the engine, refreshed on `NOTIFY config_changed`.
- The `site` tag used in examples ([F06](../features/F06-notifications.md)) becomes a real field; route matching gains `site_ids`.
- Probe failover groups (phase 2) are per site by default.
- Still open: tenant hierarchy for resellers ([F01](../features/F01-tenancy-auth-audit.md)). A CSP's end customers are not sites and not devices; they would be sub-tenants.

## Implementation note: discovered devices (M4, 2026-10-07)

Collectors create child devices through their inventory (`xapi.pool`: hosts and VMs). A discovered device has `managed_by` = the collector check's ID and `collector_key` (the collector's own key, a UUID), unique per collector, so renames and migrations update the device instead of creating one. Parents resolve from the inventory (a VM under its host, else under the collector's device). Names stay unique per tenant: a clash gets the key's start in brackets. A device the collector stops reporting gets `collector_gone_at` and is removed after 7 days; deleting the collector check removes them all. They do not count toward `max_devices` and are not billed (F13 rule 10); checks a user adds on them count and bill normally. Every creation is audited (`device.discover`, actor `system`). The API shows `discovered: {check_id, key, gone_at}` on such devices. Migration `00016`.
