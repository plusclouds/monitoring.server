# API guide

The monitoring server is API-first: everything it does is an HTTP call, and there is no other interface. This guide explains the conventions and lists every endpoint. The authoritative, machine-readable description is the OpenAPI 3.1 document [api/openapi.yaml](../../api/openapi.yaml), which the server also publishes:

| URL | Content | Authentication |
| --- | --- | --- |
| `/docs` | Interactive reference (Redoc) | none |
| `/v1/openapi.json`, `/v1/openapi.yaml` | The spec | none |
| `/healthz` | Liveness, answers `ok` | none |

If this guide and the spec ever disagree, the spec wins. For task-oriented walkthroughs, see the [usage guide](../usage-guide.md).

## Contents

- [Conventions](#conventions)
- [Authentication and roles](#authentication-and-roles)
- [Errors](#errors)
- [Pagination, filtering and updates](#pagination-filtering-and-updates)
- [External IDs](#external-ids)
- [Endpoint reference](#endpoint-reference)
- [Object shapes](#object-shapes)
- [Push ingest API](#push-ingest-api)
- [MQTT ingest](#mqtt-ingest)
- [Webhook events](#webhook-events)
- [Platform key operations](#platform-key-operations)
- [Using the spec: clients, Postman](#using-the-spec-clients-postman)

## Conventions

- **Base URL.** `https://<your host>/v1`. All paths below are relative to it unless they start with `/ingest`.
- **Format.** JSON in and out (`Content-Type: application/json`). PATCH accepts `application/merge-patch+json` or `application/json`.
- **Identifiers.** UUIDs (v7).
- **Timestamps.** RFC 3339 in UTC, for example `2026-10-10T10:15:00Z`.
- **Limits.** Request bodies up to 4 MiB (`api.max_body`). Requests time out after 90 s.
- **Request IDs.** Send `X-Request-ID` (short string) and the server echoes it in the response and in error bodies. If you do not send one, it makes one. Quote it when you report a problem.
- **Idempotency.** `PUT` and `DELETE` are idempotent. `POST` is not. Use the by-external-ID `PUT` endpoints to create-or-update safely.

## Authentication and roles

Send an API key on every request except the unauthenticated URLs above:

```
Authorization: Bearer mon_<prefix>_<secret>
```

A key is shown once, at creation. The server stores only a hash. Revoked, expired and wrong keys all get `401`.

| Key kind | Acts as | Created by |
| --- | --- | --- |
| **Tenant key** | One tenant, with a role | `monitor admin bootstrap --standalone` (first admin key) and `POST /v1/api-keys` |
| **Platform key** | The installation: provisions tenants and users, and acts inside a named tenant | `monitor admin bootstrap` without `--standalone`. Meant for a parent system such as the PlusClouds panel. See [Platform key operations](#platform-key-operations). |

Tenant roles:

| Role | Can |
| --- | --- |
| `read-only` | Read everything in the tenant (except audit log, keys) |
| `operator` | Everything above, plus acknowledge, comment, resolve incidents, run checks now, and create or change sites, devices, checks, credentials, webhooks and alert routes |
| `admin` | Everything above, plus manage API keys, members, and read the audit log |

A request outside the role gets `403 forbidden`.

API key options (`POST /api-keys`):

```json
{
  "name": "grafana-readonly",
  "role": "read-only",
  "expires_at": "2027-01-01T00:00:00Z",
  "ip_allowlist": ["203.0.113.10/32", "2001:db8::/48"]
}
```

`expires_at` and `ip_allowlist` are optional. The client address used for the allowlist, the audit log and rate limits comes from `X-Forwarded-For` only when the request arrives from a `trusted_proxies` address.

**Rate limits.** Each key has a token bucket: the tenant's `api_rate_per_minute` (default 1200 for tenants, 6000 for platform keys; 0 is unlimited). Beyond it you get `429` with `Retry-After: 1`.

## Errors

Errors are RFC 9457 problem documents with `Content-Type: application/problem+json`:

```json
{
  "type": "https://monitor.plusclouds.com/problems/not-found",
  "title": "Not found",
  "status": 404,
  "instance": "/v1/devices/0192f7c4-...",
  "request_id": "b4d1e0c2"
}
```

The `type` URI is a stable identifier. It is not a page you can open, and it uses the PlusClouds domain in every installation. Match on the last path segment, or on `status`. `detail` explains the problem in words when there is something to say.

| Status | `type` suffix | Meaning |
| --- | --- | --- |
| 400 | `invalid-header`, `tenant-required` | Malformed request, bad header, missing tenant header (platform key) |
| 401 | `unauthenticated` | Missing, wrong, revoked or expired key. Carries `WWW-Authenticate: Bearer`. |
| 403 | `forbidden`, `platform-only`, `tenant-suspended`, `not-a-member` | Role too low, platform-only operation, or a suspended tenant (reads work, writes are refused) |
| 404 | `not-found` | No such object in your tenant. Other tenants' objects are indistinguishable from missing ones. |
| 409 | various, for example `limit-reached` | Conflict: duplicate, object in use, tenant limit reached, tenant deleted |
| 422 | `invalid-value` | The body parsed but a value is not allowed: plugin config does not match its schema, interval below the minimum, network not allowed |
| 429 | `rate-limited` | Too many requests |
| 500 | `internal` | Server fault. The cause is in the server log under the `request_id`. |

## Pagination, filtering and updates

**Lists** return `{"items": [...], "next_cursor": "..."}`. Pass the cursor back as `?cursor=` for the next page; `next_cursor` is `null` on the last page. `?limit=` is 1 to 500 (default 100). Sort order is documented per endpoint (devices and checks by ID, incidents and audit events newest first). Treat cursors as opaque.

```sh
cursor=""
while :; do
  page=$(curl -sS -H "Authorization: Bearer $MON_KEY" "$MON_URL/v1/devices?limit=200&cursor=$cursor")
  echo "$page" | jq -c '.items[]'
  cursor=$(echo "$page" | jq -r '.next_cursor // empty')
  [ -z "$cursor" ] && break
done
```

**Filters** are plain query parameters. The common ones:

| Endpoint | Filters |
| --- | --- |
| `GET /devices` | `type`, `site_id`, `parent_id`, `availability` (repeatable), `tag=key=value` (repeatable, all must match), `external_source`, `external_type`, `external_id` |
| `GET /checks` | `device_id`, `plugin`, `enabled` |
| `GET /incidents` | `status` (`open`, `acknowledged`, `resolved`, `active`), `severity`, `device_id`, `check_id`, `suppressed`, `object_key` |
| `GET /audit` | `object_type`, `object_id`, `action`, `from`, `to` |
| `GET /webhooks/{id}/deliveries` | `status` (`pending`, `delivered`, `failed`, `cancelled`) |

**Updating.**

- `PUT /resource/{id}` replaces the whole writable representation. Omitted optional fields return to their defaults.
- `PATCH /resource/{id}` is a JSON Merge Patch (RFC 7396): fields you leave out stay, `null` clears an optional field, and objects such as `tags` merge key by key. The result is validated like a `PUT`.
- `DELETE` removes the object. Deleting a device deletes its checks and the devices it contains. Deleting a credential, webhook or site that is still in use returns `409`.

## External IDs

Sites, devices, credentials, webhooks and alert routes can carry a reference to the object in your own system (`source`, optional `type`, `id`). That lets you manage them idempotently without remembering server UUIDs.

```json
{ "name": "db-01", "type": "server", "address": "10.0.4.20",
  "external": { "source": "cmdb", "type": "host", "id": "srv-0042" } }
```

- `PUT /devices/by-external-id/srv-0042?source=cmdb&type=host` creates the device or replaces it.
- `PATCH /devices/by-external-id/{id}?source=...` changes one and never creates (`404` if missing).
- List endpoints filter with `external_source`, `external_type`, `external_id`.
- `source` defaults to the installation's identity source (`MONITOR_IDENTITY_SOURCE`, default `plusclouds`). Standalone users should always pass `source` explicitly, or set the identity source to their own label once, before creating objects.
- Objects created from an external ID report `managed_by: "api"`.

## Endpoint reference

Roles: blank = any role; **operator** = operator or admin; **admin** = admin; **platform** = platform key only. "Writes" in the table below follow the rule in [Authentication and roles](#authentication-and-roles): all `POST`, `PUT`, `PATCH`, `DELETE` on monitored objects need `operator`, except the items marked otherwise. The spec is exact; the column marks what it says explicitly.

### Identity and audit

| Method | Path | Purpose | Role |
| --- | --- | --- | --- |
| `GET` | `/me` | The resolved caller: key, role, tenant, acting user | |
| `GET` | `/tenant` | Your tenant, with its limits | |
| `GET` | `/api-keys` | List keys (never their secrets) | admin |
| `POST` | `/api-keys` | Create a key; the secret is in the response once | admin |
| `DELETE` | `/api-keys/{key_id}` | Revoke a key | admin |
| `GET` | `/audit` | Query the audit log (hash-chained, tamper evident) | admin |

### Plugins and credentials

| Method | Path | Purpose | Role |
| --- | --- | --- | --- |
| `GET` | `/plugins` | Plugins built into this server: config schema, metrics, intervals, credential types | |
| `GET` | `/plugins/{type}` | One plugin's manifest | |
| `GET` | `/credential-types` | Credential types and their field schemas | |
| `GET` | `/credentials` | List credentials, secrets never returned | |
| `POST` | `/credentials` | Store a credential, encrypted at rest | operator |
| `PUT` | `/credentials/by-external-id/{external_id}` | Create or replace | operator |
| `GET`, `PUT`, `DELETE` | `/credentials/{credential_id}` | One credential; delete only when no check uses it | operator for writes |

### Sites and devices

| Method | Path | Purpose | Role |
| --- | --- | --- | --- |
| `GET`, `POST` | `/sites` | List, add | operator for POST |
| `PUT`, `PATCH` | `/sites/by-external-id/{external_id}` | Create or replace, change | operator |
| `GET`, `PUT`, `PATCH`, `DELETE` | `/sites/{site_id}` | One site; delete only without devices | operator for writes |
| `GET`, `POST` | `/devices` | List, add | operator for POST |
| `PUT`, `PATCH` | `/devices/by-external-id/{external_id}` | Create or replace, change | operator |
| `GET`, `PUT`, `PATCH`, `DELETE` | `/devices/{device_id}` | One device | operator for writes |
| `GET` | `/devices/{device_id}/children` | Devices this device contains | |
| `GET`, `POST` | `/devices/{device_id}/dependencies` | What the device needs; record a dependency (`{"depends_on_id": "..."}`) | operator for POST |
| `DELETE` | `/devices/{device_id}/dependencies/{depends_on_id}` | Remove a stored dependency | operator |
| `GET` | `/devices/{device_id}/dependents` | Devices that need this one | |
| `GET` | `/devices/{device_id}/impact` | Everything suppressed if this device failed | |
| `POST` | `/devices/{device_id}/test` | Run every enabled check once and return the results | operator |

Device types: `network`, `server`, `bmc`, `camera`, `web`, `hypervisor_pool`, `hypervisor_host`, `vm`, `iot`, `ups`, `pdu`, `sensor`, `llm_endpoint`, `llm_application`, `other`.

### Checks, state and incidents

| Method | Path | Purpose | Role |
| --- | --- | --- | --- |
| `GET`, `POST` | `/devices/{device_id}/checks` | The device's checks; add one | operator for POST |
| `GET` | `/checks` | List checks, filtered | |
| `GET`, `PUT`, `PATCH`, `DELETE` | `/checks/{check_id}` | One check | operator for writes |
| `GET` | `/checks/{check_id}/state` | Current state: status, output, since when | |
| `GET` | `/checks/{check_id}/objects` | Objects of a collector check (interfaces, fans, VMs) and their state | |
| `POST` | `/checks/{check_id}/run-now` | Run the check at once | operator |
| `POST` | `/checks/{check_id}/rotate-token` | New ingest token for a push check; the old one stops at once | operator |
| `GET`, `PUT`, `DELETE` | `/checks/{check_id}/whoopsy` | Adaptive band alerting: read, enable or change, disable | operator for writes |
| `GET` | `/checks/{check_id}/whoopsy/band` | The band of every result, for graphs | |
| `POST` | `/checks/{check_id}/whoopsy/reset` | Forget the learned band | operator |
| `GET` | `/incidents` | List incidents, newest first | |
| `GET` | `/incidents/{incident_id}` | One incident, with comments | |
| `POST` | `/incidents/{incident_id}/ack` | Acknowledge | operator |
| `POST` | `/incidents/{incident_id}/resolve` | Resolve by hand | operator |
| `POST` | `/incidents/{incident_id}/comments` | Comment (`{"body": "..."}`) | operator |

### Notifications

| Method | Path | Purpose | Role |
| --- | --- | --- | --- |
| `GET`, `POST` | `/webhooks` | List, add endpoints. The signing secret is returned once. | operator for POST |
| `PUT` | `/webhooks/by-external-id/{external_id}` | Create or replace | operator |
| `GET`, `PUT`, `DELETE` | `/webhooks/{webhook_id}` | One endpoint; delete only when no route uses it | operator for writes |
| `POST` | `/webhooks/{webhook_id}/test` | Send a signed `monitoring.webhook.test` event now, return the receiver's answer | operator |
| `POST` | `/webhooks/{webhook_id}/rotate-secret` | New signing secret; the old one is also valid for 24 h | operator |
| `GET` | `/webhooks/{webhook_id}/deliveries` | The delivery log | |
| `POST` | `/webhooks/{webhook_id}/deliveries/{delivery_id}/replay` | Send one delivery again | operator |
| `POST` | `/webhooks/{webhook_id}/deliveries/replay` | Send failed deliveries again in bulk (`?status=failed&from=&to=`, up to 10,000) | operator |
| `GET`, `POST` | `/alert-routes` | List in evaluation order, add | operator for POST |
| `PUT` | `/alert-routes/by-external-id/{external_id}` | Create or replace | operator |
| `GET`, `PUT`, `DELETE` | `/alert-routes/{route_id}` | One route | operator for writes |
| `POST` | `/alert-routes/test` | Show which routes a sample incident would reach | |

### Metrics and usage

| Method | Path | Purpose | Role |
| --- | --- | --- | --- |
| `GET` | `/metrics/series` | Series of a device or check (`device_id` or `check_id` required) | |
| `GET` | `/metrics/query` | Points of the selected series | |
| `GET` | `/metrics/summary` | Statistics over a window: count, min, max, avg, stddev, p50, p95, p99, last | |
| `GET` | `/metrics/retention-classes` | Retention classes and how long each level is kept | |
| `PUT` | `/metrics/retention-classes/{name}` | Create or change a class | platform |
| `GET` | `/usage/hours`, `/usage/current`, `/usage/weights` | Billing-style usage meter of the tenant. Relevant only if you bill your own users. | |
| `GET` | `/usage/tenants` | Usage of every tenant | platform |

Standalone installs without a platform key set retention with `monitor admin retention` ([deployment guide](../deployment/standalone-vm.md#data-retention)).

### MQTT ingest management

| Method | Path | Purpose |
| --- | --- | --- |
| `GET`, `POST` | `/ingest/mqtt-credentials` | List, create (the password is returned once) |
| `GET`, `PATCH`, `DELETE` | `/ingest/mqtt-credentials/{credential_id}` | One credential; rename, enable, disable, change flags |
| `POST` | `/ingest/mqtt-credentials/{credential_id}/rotate` | New password, shown once |
| `GET` | `/ingest/profiles` | Built-in payload profiles (`json`, `fixlean-esp`) |
| `GET` | `/ingest/unregistered` | Device keys whose messages were dropped |
| `GET`, `POST`, `DELETE` | `/devices/{device_id}/mqtt` | Binding and live session; pre-register; unbind |

## Object shapes

Only the fields you most often need. The spec lists all of them.

### Device

```json
{
  "id": "0192f7c4-...", "name": "db-01", "address": "10.0.4.20", "type": "server",
  "tags": { "rack": "a12" }, "notes": null,
  "parent_id": null, "site_id": "...", "physical_peer_id": null,
  "inventory": {}, "managed_by": "api", "status": { },
  "external": { "source": "cmdb", "type": "host", "id": "srv-0042" },
  "discovered": null, "created_at": "...", "updated_at": "..."
}
```

Write fields (`POST`, `PUT`): `name` (required), `type` (required), `address`, `tags` (up to 50, values up to 200 chars), `notes`, `parent_id` (container, for example the hypervisor of a VM), `site_id`, `physical_peer_id` (linked BMC or host), `external`.

`status` carries the derived availability and the open incident count. `discovered` is set for devices that a collector created (VMs, pool hosts); those follow their collector, do not count toward `max_devices`, and disappear 7 days after the collector stops reporting them.

### Check

```json
{
  "id": "...", "device_id": "...", "name": "Homepage", "plugin": "http",
  "config": { "keyword": "Welcome" },
  "interval_seconds": 60, "timeout_seconds": null, "enabled": true,
  "thresholds": [ { "id": "r1", "metric": "total_ms",
                    "warning": {"op": ">", "value": 1500},
                    "critical": {"op": ">", "value": 5000},
                    "for": "5m", "hysteresis": 100 } ],
  "failure_count": 3, "recovery_count": 1,
  "is_host_check": true, "unknown_is_critical": false, "runbook_url": null,
  "credentials": { "auth": "<credential id>" },
  "whoopsy": null, "push": null, "managed_by": "api"
}
```

- `config` is validated against the plugin's `config_schema` (`GET /plugins/{type}`); a mismatch is `422`.
- `interval_seconds` is 1 to 86400. It must meet both the plugin's `min_interval_seconds` and the tenant's `min_check_interval_seconds`.
- `failure_count` consecutive bad results are needed before a state change (default 3; 1 for push checks). `recovery_count` for the way back.
- Threshold `op` is one of `>`, `>=`, `<`, `<=`, `==`, `!=`, `between`, `outside` (the last two with `value_max`). Rules on collector checks can set `object` to `*` or one object key.
- `credentials` maps a role (usually `auth`) to a credential ID.

### Incident

`id`, `check_id`, `device_id`, `object_key`, `severity` (`warning` or `critical`), `status` (`open`, `acknowledged`, `resolved`), `summary`, `last_output`, `flapping`, `suppressed`, `root_incident_id`, `root_device_id`, `opened_at`, `acknowledged_at`, `resolved_at`, `resolved_by` (`recovery`, `manual`, `check-deleted`, `check-disabled`, `object-gone`).

### Metrics query

```
GET /v1/metrics/query?check_id=<id>&name=total_ms&name=ttfb_ms
    &from=2026-10-10T00:00:00Z&to=2026-10-10T12:00:00Z&step=300&agg=avg
```

| Parameter | Meaning |
| --- | --- |
| `device_id` or `check_id` | Required: which series |
| `name` | Metric name; repeat for several |
| `object` | Object key of a collector check |
| `from`, `to` | Window; default the last hour |
| `step` | Bucket seconds; default about 500 points, at least 10 s |
| `agg` | `avg` (default), `min`, `max`, `sum`; `stddev`, `p50`, `p95`, `p99`, `p<n>[.fff]` use raw samples and need `from` inside raw retention |
| `resolution` | `raw`, `5m`, `1h`; default picks by `step` and retention |
| `moving_window` | 1 to 1000: smooth after aggregation |

At most 10,000 points per series. Buckets start at multiples of `step` counted from 2000-01-01T00:00:00Z, so the first one may begin before `from`.

## Push ingest API

The ingest listener is separate from the management API (default port 9443, behind your proxy at `/ingest/`). It authenticates with the check's own push token, not an API key.

```
POST /ingest/v1/{check_id}
Authorization: Bearer mpush_<8>_<43>
Content-Type: application/json
```

| Item | Detail |
| --- | --- |
| Body | One JSON object, or an array of up to 1,000 objects (oldest first after sorting by timestamp). Up to 64 KiB. |
| Mapping | Defined in the check's `config`: `metrics` (name to JSONPath-subset selector), `units`, `status`, `output`, `timestamp`, `missed_count` (default 3). |
| Timestamps | Accepted from 24 hours in the past to 5 minutes ahead. Outside, or absent, the server time is used. Results older than 2 x the interval update metrics but not state. |
| Rate | 2 requests/s per token, burst 20 (`ingest.http.rate_limit`). |
| Token | Returned once as `push_token` in the response that creates the check (or `rotate-token`). Only its SHA-256 is stored. |

| Answer | Meaning |
| --- | --- |
| `202 {"accepted": n}` | Taken |
| `400` | Malformed message or mapping failed |
| `401` | Missing or wrong token (the same for an unknown check) |
| `409` | Check disabled |
| `413` | Body too large |
| `429` | Rate exceeded |

Example:

```sh
curl -X POST "$MON_URL/ingest/v1/$CHECK_ID" \
  -H "Authorization: Bearer $PUSH_TOKEN" -H "Content-Type: application/json" \
  -d '[{"ts":"2026-10-10T10:00:00Z","sensors":{"temp":21.4}},{"ts":"2026-10-10T10:01:00Z","sensors":{"temp":21.6}}]'
```

Silence is the failure: if no message arrives for `interval x missed_count`, the check goes CRITICAL with "no data since ...", and recovers on the next message.

## MQTT ingest

With `MONITOR_MQTT_ENABLED=true`, the engine runs an MQTT 3.1.1 and 5 broker on TLS port 8883.

| Item | Detail |
| --- | --- |
| Auth | Username and password from `/ingest/mqtt-credentials`. The credential decides the tenant. |
| Topic | `<prefix>/<device_key>/...`. The second segment is the device key (a MAC or serial), compared without case. |
| Direction | Devices publish; nothing may subscribe. Messages are acknowledged (QoS 1) after the pipeline takes them. Clean sessions only. |
| Payload | `json` profile: every numeric field becomes a metric; booleans become 1 and 0; nested objects join with `_` (three levels). |
| Limits | 64 KiB packets; 10 messages/s per client (burst 50); 5 connects/s per source IP (burst 20). |
| Presence | An `mqtt.connection` check follows connect, disconnect and keepalive timeout (1.5 x keepalive), so offline detection does not wait for silence timers. |

## Webhook events

The server's only outbound notification is a signed HTTP POST per event, using the CloudEvents 1.0 envelope and the [Standard Webhooks](https://www.standardwebhooks.com/) signature scheme.

**Request**

```
POST <your url>
Content-Type: application/json
webhook-id: 0192f7c4-...
webhook-timestamp: 1791626100
webhook-signature: v1,K5oZfzN95Z9UVu1EsfQmfVNQhnkZ2/3HmN2SFXTqCmU=
<your static headers>
```

Verification: HMAC-SHA256 over `"{webhook-id}.{webhook-timestamp}.{raw body}"` with the key `base64decode(secret without "whsec_")`, base64-encoded, compared in constant time with each `v1,` value in the header. During a secret rotation there are two signatures. Reject timestamps older than a few minutes. A Python example is in the [usage guide](../usage-guide.md#receive-and-verify-events).

**Envelope**

```json
{
  "specversion": "1.0",
  "id": "0192f7c4-...",
  "source": "/monitoring/<installation_id>/<tenant_id>",
  "type": "monitoring.incident.opened",
  "time": "2026-10-10T10:15:00Z",
  "datacontenttype": "application/json",
  "subject": "<incident id>",
  "data": {
    "account_id": null,
    "object_type": "Monitoring\\Incidents",
    "object": { },
    "device": { },
    "check": { },
    "route": { "labels": { } },
    "actor": null,
    "links": { "incident": "https://..." }
  }
}
```

`id` equals the `webhook-id` header and is identical on every retry. `data.account_id` is the PlusClouds account UUID and is `null` in standalone installs. `data.object_type` has a PHP-style name for historical reasons and can be ignored.

**Event types**

| Type | When |
| --- | --- |
| `monitoring.incident.opened` | An incident opened (or stopped being suppressed) |
| `monitoring.incident.updated` | Severity or output changed |
| `monitoring.incident.acknowledged` | Someone acknowledged it (`data.actor`) |
| `monitoring.incident.resolved` | It recovered or was resolved |
| `monitoring.incident.commented` | A comment was added |
| `monitoring.incident.renotify` | A route's `repeat_interval_seconds` elapsed on an open, unacknowledged incident |
| `monitoring.incident.escalated` | A later route step came due (`data.route.step`) |
| `monitoring.tenant.device_limit_reached`, `monitoring.tenant.check_limit_reached` | A create brought the tenant to its limit |
| `monitoring.webhook.test` | You called `/webhooks/{id}/test` |
| `monitoring.heartbeat` | Dead man's switch to the configured heartbeat URLs (not through routes) |

**Grouped events.** When a route groups incidents, one event carries `data.objects` (each incident's `object`, `device`, `check`) and `data.group` (`id`, `key`, `count`); `data.object`, `data.device` and `data.check` are null. A group of one is sent as a plain event.

**Delivery.** At least once. Answer with a 2xx within the endpoint's `timeout_seconds` (default 10, max 30). Anything else is retried after 5 s, 30 s, 2 min, 10 min, 30 min, then hourly, for 24 hours; events of one incident stay in order. Every attempt, with the receiver's status and the first KiB of its answer, is in the delivery log. A `410 Gone` disables the endpoint. Deduplicate on `webhook-id`.

**Network policy.** Webhook URLs resolving into blocked networks (private ranges by default; see `allowed_target_networks`) are refused with `422` when saved, and again at delivery time.

## Platform key operations

Needed only if a parent system (a hosting panel, a billing system) manages many tenants through this server. Standalone single-tenant installs can skip this section.

The platform key (`monitor admin bootstrap` without `--standalone`) is not tied to a tenant. It can call the `platform` endpoints:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/tenants` | List tenants |
| `PUT` | `/tenants/by-external-id/{external_id}` | Create or update a tenant by your ID, with name and limits |
| `PATCH` | `/tenants/by-external-id/{external_id}` | Change name, status (suspend) or limits |
| `DELETE` | `/tenants/by-external-id/{external_id}` | Soft-delete; data is purged after `platform.tenant_purge_grace` (30 days) |
| `POST` | `/tenants/by-external-id/{external_id}/restore` | Undelete |
| `GET` | `/tenants/{tenant_id}/members` | Members |
| `PUT`, `DELETE` | `/tenants/{tenant_id}/members/by-external-id/{user_external_id}` | Add or change a member's role, remove one |
| `PATCH` | `/users/by-external-id/{external_id}` | Rename or disable a user everywhere |
| `PUT` | `/metrics/retention-classes/{name}` | Retention classes |
| `GET` | `/usage/tenants` | Usage of every tenant per closed hour |

Tenant limits (`max_devices`, `max_checks`, `max_webhooks`, `max_alert_routes`, `min_check_interval_seconds`, `api_rate_per_minute`, `allowed_target_networks`, `metric_classes`, `max_api_key_lifetime_days`) are set here.

To act inside a tenant, add headers to any ordinary request:

| Header | Meaning |
| --- | --- |
| `X-Tenant-External-ID` or `X-Tenant-ID` | Which tenant. Send one, not both. |
| `X-Actor-External-ID` | Optional: the user on whose behalf the call runs. Their role in that tenant applies and the audit log names them. Without it the call has full rights in the tenant as "platform". |

Unknown tenants and users named this way are created on the fly (just-in-time provisioning, `platform.jit`). Disable that in the config if you do not want it.

Example: create a tenant and an admin key for it, from an installation that was bootstrapped with a platform key:

```sh
curl -X PUT "$MON_URL/v1/tenants/by-external-id/acme" \
  -H "Authorization: Bearer $PLATFORM_KEY" -H "Content-Type: application/json" \
  -d '{"name":"Acme Corp","limits":{"max_devices":200,"allowed_target_networks":["10.0.0.0/8"]}}'

curl -X POST "$MON_URL/v1/api-keys" \
  -H "Authorization: Bearer $PLATFORM_KEY" -H "X-Tenant-External-ID: acme" \
  -H "Content-Type: application/json" -d '{"name":"acme admin","role":"admin"}'
```

(Request body fields for tenants: see `TenantUpsert` and `TenantLimits` in the spec.)

## Using the spec: clients, Postman

- **Generate a client** from `/v1/openapi.yaml` with any OpenAPI 3.1 generator, for example `openapi-generator-cli generate -i openapi.yaml -g python -o client/`, or `oapi-codegen` for Go (the server itself is generated from this file with the config in [api/oapi-codegen.yaml](../../api/oapi-codegen.yaml)). Some generators still lack full 3.1 support; if yours fails on nullable types, try a newer version.
- **Postman or Insomnia.** Import [api/postman/monitoring.postman_collection.json](../../api/postman/monitoring.postman_collection.json) and [api/postman/local.postman_environment.json](../../api/postman/local.postman_environment.json). The collection is written for a platform key. For a standalone tenant, put your admin key in the `platformKey` variable (the name is historical) and skip the provisioning folder, whose calls need a platform key. From the command line: `npx newman run ... --env-var platformKey=mon_...`.
- **Browse.** `https://<your host>/docs`.
- **Stability.** The spec's `info.version` is `0.1.0`. The API is under `/v1`; this release is a pre-1.0 milestone, so check the spec after each upgrade.
