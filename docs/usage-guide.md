# Usage guide

How to use a running monitoring server: add what you want to watch, get alerted, read the data. Everything here uses the REST API with `curl`, because the API is the only interface. The server has no built-in UI. Grafana and your own scripts are the clients.

To install the server, see [Deploying on a new VM](deployment/standalone-vm.md). For every endpoint and field, see the [API guide](api/README.md).

The examples assume:

```sh
export MON_URL=https://monitor.example.com        # or http://127.0.0.1:8443 on the VM
export MON_KEY=mon_...                            # your admin API key
alias mon='curl -sS -H "Authorization: Bearer $MON_KEY" -H "Content-Type: application/json"'
```

(Alias lines work in bash and zsh. In fish use a function.) Pipe output through `jq` for readability.

## The model in five lines

- A **tenant** is your account: everything below belongs to one. A standalone install has one.
- A **device** is a thing you watch: a server, a switch, a website, a camera, a sensor.
- A **check** runs one **plugin** against a device on an interval (ping, HTTP request, SNMP read, Redfish health). Plugins that receive data instead of polling are *push* checks.
- A check produces a **state** (OK, WARNING, CRITICAL, UNKNOWN) and **metrics**. Failing states open **incidents**.
- Incident changes become signed **webhook** events. **Alert routes** decide which events go to which webhook.

Noise control is built in: a check must fail several times in a row before it alerts (`failure_count`, default 3), flapping is detected, and if a device's host or parent is down, the incidents it explains are suppressed.

## 1. See what the server can do

```sh
mon $MON_URL/v1/me           # who you are, your role
mon $MON_URL/v1/tenant       # your limits
mon $MON_URL/v1/plugins | jq -r '.items[] | "\(.type)\t\(.kind)\t\(.description)"'
mon $MON_URL/v1/plugins/http | jq .config_schema
```

Built-in plugins include:

| Plugin | Use it for |
| --- | --- |
| `icmp` | Ping: round-trip time and packet loss |
| `http` | Website or API: status code, keyword, timing breakdown |
| `snmp.get`, `snmp.system`, `snmp.interfaces`, `snmp.ups`, `snmp.pdu`, `snmp.sensor` | Network gear, UPS, PDUs, environment sensors |
| `redfish.health` | Server hardware through the BMC: temperatures, fans, PSUs, disks |
| `xapi.pool` | XenServer/XCP-ng pools, their hosts and VMs |
| `camera.snapshot`, `rtsp.stream` | IP cameras |
| `push.http`, `push.mqtt` | Devices that send data to the server |

`vm.agent` and `box.agent` are PlusClouds-specific ingesters and are of no use outside PlusClouds. `monitor.self` is the server's own health check.

Each plugin publishes a JSON Schema for its `config`, its metrics, and its default and minimum interval.

## 2. Monitor a website

Create a device, then add an HTTP check.

```sh
DEVICE=$(mon -X POST $MON_URL/v1/devices -d '{
  "name": "Company website",
  "type": "web",
  "address": "https://www.example.com"
}' | jq -r .id)

mon -X POST $MON_URL/v1/devices/$DEVICE/checks -d '{
  "name": "Homepage",
  "plugin": "http",
  "config": { "keyword": "Welcome", "expected_status": [200] },
  "interval_seconds": 60,
  "is_host_check": true,
  "thresholds": [
    { "metric": "total_ms",
      "warning":  { "op": ">", "value": 1500 },
      "critical": { "op": ">", "value": 5000 },
      "for": "5m" }
  ]
}'
```

- `is_host_check` marks the check that says whether the device is up. Other checks of the device are suppressed while it fails.
- Thresholds apply to the plugin's metrics (`GET /v1/plugins/http` lists them). `for` requires the condition to hold that long. `hysteresis` adds a margin before the state clears.
- The check status itself (status code wrong, keyword missing, timeout) is decided by the plugin, independent of thresholds.

Run it now and read the result:

```sh
CHECK=<check id from the response>
mon -X POST $MON_URL/v1/checks/$CHECK/run-now
mon $MON_URL/v1/checks/$CHECK/state
mon -X POST $MON_URL/v1/devices/$DEVICE/test       # runs every enabled check once
```

If the check says the address is not allowed, your tenant's `allowed_target_networks` does not include it. See the deployment guide.

## 3. Monitor a server with ping

```sh
SRV=$(mon -X POST $MON_URL/v1/devices -d '{
  "name": "db-01", "type": "server", "address": "10.0.4.20",
  "tags": { "rack": "a12", "env": "prod" }
}' | jq -r .id)

mon -X POST $MON_URL/v1/devices/$SRV/checks -d '{
  "name": "Ping", "plugin": "icmp", "interval_seconds": 30, "is_host_check": true,
  "thresholds": [
    { "metric": "packet_loss_percent", "warning": {"op": ">", "value": 10}, "critical": {"op": ">=", "value": 100} }
  ]
}'
```

Tags are free text and are what alert routes and Grafana filter on.

## 4. Network gear, hardware, hypervisors: credentials

Plugins that log in (SNMP, Redfish, XAPI, cameras, authenticated HTTP) take a **credential**. Secrets are encrypted at rest and write-only: no endpoint returns them.

```sh
mon $MON_URL/v1/credential-types | jq              # types and their fields

CRED=$(mon -X POST $MON_URL/v1/credentials -d '{
  "name": "core switches v2c",
  "type": "<a type from credential-types>",
  "fields": { "...": "..." }                        # see fields_schema of that type
}' | jq -r .id)

mon -X POST $MON_URL/v1/devices/$DEVICE/checks -d '{
  "name": "Interfaces", "plugin": "snmp.interfaces", "interval_seconds": 60,
  "credentials": { "auth": "'$CRED'" }
}'
```

Each type lists its fields in `fields_schema` (secret fields are `writeOnly`), and each plugin lists the `credential_types` it accepts in `GET /v1/plugins/{type}`. A check created without a required credential reports UNKNOWN until one is assigned. One credential can serve many checks; it cannot be deleted while a check uses it.

Collector plugins (`snmp.interfaces`, `redfish.health`, `xapi.pool`, cameras) create **objects**: one per interface, fan, VM and so on. Incidents and thresholds work per object, and you can list them:

```sh
mon $MON_URL/v1/checks/$CHECK/objects
```

`xapi.pool` also discovers hypervisor hosts and VMs and creates them as child devices.

## 5. Devices that push data

For sensors, scripts and agents that report on their own, silence is the failure: no data for `interval × missed_count` makes the check CRITICAL.

### HTTP push

```sh
mon -X POST $MON_URL/v1/devices -d '{"name":"Boiler room","type":"sensor"}'   # note the id

mon -X POST $MON_URL/v1/devices/$DEVICE/checks -d '{
  "name": "Boiler telemetry",
  "plugin": "push.http",
  "interval_seconds": 60,
  "config": {
    "metrics": { "temperature": "$.sensors.temp", "humidity": "$.sensors.hum" },
    "units":   { "temperature": "celsius", "humidity": "percent" },
    "missed_count": 3
  },
  "thresholds": [ { "metric": "temperature", "critical": { "op": ">", "value": 45 } } ]
}'
```

The response contains `push_token` (`mpush_...`) **once**. Then the device sends:

```sh
curl -X POST $MON_URL/ingest/v1/$CHECK \
  -H "Authorization: Bearer mpush_..." -H "Content-Type: application/json" \
  -d '{"sensors": {"temp": 21.4, "hum": 48}}'
# 202 {"accepted":1}
```

- Send one object or an array of up to 1,000 with a `timestamp` for each (configure the field with `timestamp`). Timestamps up to 24 hours old or 5 minutes ahead are kept.
- Selectors are a JSONPath subset: `$.a.b`, `$.list[0].v`, `$["name with-dash"]`.
- Set `status` in the config to take the state from a field (`ok`, `warning`, `critical`, `0` to `3`, `up`, `down`, `true`, `false`).
- Rate limit is 2 requests per second per token (burst 20), and bodies are limited to 64 KiB.
- Rotate or revoke a token without touching other devices: `POST /v1/checks/{id}/rotate-token`.

Answers: `202` accepted, `401` bad token, `400` malformed body, `409` check disabled, `413` too big, `429` too fast.

### MQTT sensors

Requires the MQTT broker (deployment guide, section 7).

```sh
# Credential for one device (the key is usually the MAC or serial)
mon -X POST $MON_URL/v1/ingest/mqtt-credentials -d '{
  "name": "greenhouse-1", "kind": "device", "device_key": "AA:BB:CC:DD:EE:01", "profile": "json"
}'
# -> returns username and password once
```

The device connects to `mqtts://mqtt.example.com:8883` with that username and password, and publishes JSON to `<prefix>/<device_key>/<anything>`. The second topic segment is the device key; a device credential may publish only under its own key.

- With `auto_register` true (default), the first message creates the device with a `push.mqtt` data check and an `mqtt.connection` check. Online/offline follows the MQTT connection and keepalive.
- The `json` profile turns every numeric field into a metric (nested objects joined with `_`).
- Use `kind: "shared"` for fleets that share one login; the device key then comes only from the topic.
- `GET /v1/ingest/unregistered` lists keys whose messages were dropped (auto-registration off or limit reached). `POST /v1/devices/{id}/mqtt` pre-registers a device.

## 6. Get alerts: webhooks and routes

The server delivers one thing: signed HTTP POSTs. What happens next (email, Slack, SMS, a ticket) is the receiver's job. n8n, Node-RED, Zapier, a small script or an incident tool all work.

### Create a webhook

```sh
WH=$(mon -X POST $MON_URL/v1/webhooks -d '{
  "name": "n8n alerts", "url": "https://n8n.example.com/webhook/monitoring", "timeout_seconds": 10
}')
echo "$WH" | jq        # note id and the signing secret (whsec_...), shown once
```

Add static headers (for example an auth token the receiver wants) with `"headers": {"X-Api-Token": "..."}`. They are stored encrypted. Check delivery end to end:

```sh
mon -X POST $MON_URL/v1/webhooks/<id>/test        # sends a signed monitoring.webhook.test event
```

### Create a route

No route, no alerts. A route says "incidents matching this go to that webhook".

```sh
mon -X POST $MON_URL/v1/alert-routes -d '{
  "name": "critical to n8n",
  "endpoint_id": "<webhook id>",
  "match": { "severity": ["critical"], "device_types": ["server", "network"], "tags": { "env": "prod" } },
  "group_by": ["root_device_id"],
  "group_wait_seconds": 30,
  "repeat_interval_seconds": 14400,
  "labels": { "recipient": "noc" },
  "steps": [
    { "after_seconds": 0 },
    { "after_seconds": 900, "labels": { "recipient": "on-call" }, "only_if_unacknowledged": true }
  ]
}'
```

- Routes are evaluated in order (`position`). The first match wins unless `continue` is true. An empty `match` takes everything, so a catch-all route is one call.
- `group_by` bundles incidents with the same value into one event, so a switch failure sends one message.
- `repeat_interval_seconds` (300 to 604800) re-sends open, unacknowledged incidents.
- `steps` escalate: each later step is sent that many seconds after the first, unless the incident was resolved (or acknowledged, with `only_if_unacknowledged`). `labels` are passed to the receiver, which decides what they mean.
- Try a route without waiting for a real incident: `POST /v1/alert-routes/test`.

### Receive and verify events

Events use the CloudEvents 1.0 envelope, signed with the [Standard Webhooks](https://www.standardwebhooks.com/) scheme. Libraries exist for most languages. Headers:

| Header | Meaning |
| --- | --- |
| `webhook-id` | Event ID. Identical on retries: deduplicate on it. |
| `webhook-timestamp` | Unix seconds. Reject old ones. |
| `webhook-signature` | `v1,<base64 HMAC-SHA256>`; several space-separated values while a secret rotates. |

The signed content is `webhook-id + "." + webhook-timestamp + "." + raw body`; the key is the base64 part of `whsec_<base64>`. A minimal Python receiver check:

```python
import base64, hashlib, hmac, time

def verify(secret: str, headers: dict, body: bytes, tolerance=300) -> bool:
    if abs(time.time() - int(headers["webhook-timestamp"])) > tolerance:
        return False
    key = base64.b64decode(secret.removeprefix("whsec_"))
    signed = f'{headers["webhook-id"]}.{headers["webhook-timestamp"]}.'.encode() + body
    expected = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest()).decode()
    return any(hmac.compare_digest(expected, s.split(",", 1)[1])
               for s in headers["webhook-signature"].split() if s.startswith("v1,"))
```

Event types: `monitoring.incident.opened`, `.updated`, `.acknowledged`, `.resolved`, `.commented`, `.renotify`, `.escalated`, plus `monitoring.tenant.device_limit_reached`, `monitoring.tenant.check_limit_reached` and `monitoring.webhook.test`. Example:

```json
{
  "specversion": "1.0",
  "id": "0192f7c4-...",
  "source": "/monitoring/<installation_id>/<tenant_id>",
  "type": "monitoring.incident.opened",
  "time": "2026-10-10T10:15:00Z",
  "subject": "<incident id>",
  "data": {
    "object": { "severity": "critical", "summary": "...", "...": "..." },
    "device": { "name": "db-01", "address": "10.0.4.20", "tags": { "rack": "a12" } },
    "check":  { "name": "Ping", "plugin": "icmp", "thresholds": [ ] },
    "route":  { "labels": { "recipient": "noc" } },
    "links":  { "incident": "https://..." }
  }
}
```

(Fields are abbreviated. Capture a real event, from a test delivery or an incident, for the full shape.) Set `notifier.incident_url_template` in the server config to get a `data.links.incident` link into your own tool.

Delivery is at least once. The server retries failures with growing delays for 24 hours, keeps order per incident, and records every attempt:

```sh
mon "$MON_URL/v1/webhooks/<id>/deliveries"                        # the delivery log
mon -X POST "$MON_URL/v1/webhooks/<id>/deliveries/<did>/replay"   # send one again
mon -X POST "$MON_URL/v1/webhooks/<id>/deliveries/replay?status=failed"   # bulk
mon -X POST $MON_URL/v1/webhooks/<id>/rotate-secret               # old secret stays valid 24 h
```

A receiver that answers `410 Gone` has its webhook disabled by the server (`disabled_reason`).

## 7. Work with incidents

```sh
mon "$MON_URL/v1/incidents?status=active"
mon "$MON_URL/v1/incidents?status=active&severity=critical&suppressed=false"
mon $MON_URL/v1/incidents/<id>                                    # with comments
mon -X POST $MON_URL/v1/incidents/<id>/ack
mon -X POST $MON_URL/v1/incidents/<id>/comments -d '{"body":"Rebooting the node"}'
mon -X POST $MON_URL/v1/incidents/<id>/resolve
```

Incidents resolve themselves when the check recovers (`recovery_count` good results, default 1). `suppressed: true` means an upstream failure explains the incident, so it was not notified. Tell the server how your devices depend on each other for better suppression:

```sh
mon -X POST $MON_URL/v1/devices/$SRV/dependencies -d '{"depends_on_id":"<switch id>"}'
mon $MON_URL/v1/devices/<switch id>/impact          # everything that goes quiet if the switch fails
```

Put a VM or camera inside its host or NVR with `parent_id` when creating the device. Children are suppressed when the container fails.

## 8. Read metrics

```sh
# Which series exist for a check
mon "$MON_URL/v1/metrics/series?check_id=$CHECK"

# Average response time per 5 minutes over the last day
mon "$MON_URL/v1/metrics/query?check_id=$CHECK&name=total_ms&from=$(date -u -d '1 day ago' +%FT%TZ)&step=300&agg=avg"

# 95th percentile and stats over a window
mon "$MON_URL/v1/metrics/query?check_id=$CHECK&name=total_ms&agg=p95&step=3600&from=2026-10-09T00:00:00Z"
mon "$MON_URL/v1/metrics/summary?check_id=$CHECK&name=total_ms"
```

- `name` can repeat; `object` selects an object of a collector check; `device_id` selects every check of a device.
- Without `resolution` the server picks raw data, 5-minute or hourly rollups from the step and retention. A response holds at most 10,000 points per series.
- `avg`, `min`, `max`, `sum` work at every resolution. `stddev` and percentiles (`p50`, `p95`, `p99.9`) use raw samples only, so `from` must be within raw retention.
- `moving_window` smooths the line.

For dashboards, use Grafana against PostgreSQL ([deploy/grafana](../deploy/grafana/README.md)) or call this endpoint from your own front end.

## 9. Adaptive alerts: Whoopsy!

Fixed thresholds do not fit metrics that have a daily rhythm. Whoopsy! learns a band of normal values for a check's metric and alerts when results leave it.

```sh
mon $MON_URL/v1/checks/$CHECK/whoopsy                  # current settings and band
mon -X PUT $MON_URL/v1/checks/$CHECK/whoopsy -d '{"window": 12, "deviations": 2, "consecutive": 3, "direction": "above", "severity": "warning"}'
mon "$MON_URL/v1/checks/$CHECK/whoopsy/band?from=...&to=..."   # band for graphs
mon -X POST $MON_URL/v1/checks/$CHECK/whoopsy/reset    # accept a new normal
mon -X DELETE $MON_URL/v1/checks/$CHECK/whoopsy
```

Every field is optional. Defaults: the plugin's default metric (`whoopsy_metric` in `GET /v1/plugins`), a window of 7 results, 1 standard deviation, 3 results in a row, direction `above`, severity `warning`. Whoopsy! is refused on collector checks, and it changes how the check is billed on PlusClouds (irrelevant to you).

## 10. Organize with sites

```sh
SITE=$(mon -X POST $MON_URL/v1/sites -d '{"name":"Istanbul DC 1","timezone":"Europe/Istanbul"}' | jq -r .id)
mon -X PATCH $MON_URL/v1/devices/$SRV -d '{"site_id":"'$SITE'"}'
```

Routes can match `site_ids`, and lists can filter by site.

## 11. People and automation: API keys

Roles: `read-only` reads everything; `operator` also acknowledges, comments, resolves, runs checks and edits what is monitored; `admin` also manages keys and reads the audit log.

```sh
mon -X POST $MON_URL/v1/api-keys -d '{
  "name": "grafana-readonly", "role": "read-only",
  "expires_at": "2027-01-01T00:00:00Z", "ip_allowlist": ["203.0.113.10/32"]
}'                                                    # the key is returned once
mon $MON_URL/v1/api-keys
mon -X DELETE $MON_URL/v1/api-keys/<key id>
mon "$MON_URL/v1/audit?limit=50"                      # who did what, tamper-evident
```

Use one key per integration, so you can revoke one without touching the rest.

## 12. Infrastructure as code

Every resource that you create with a script can carry an `external` reference (`source`, `type`, `id`) and be addressed by it with an idempotent `PUT .../by-external-id/{id}`. That gives you create-or-update semantics without storing the server's UUIDs:

```sh
mon -X PUT "$MON_URL/v1/devices/by-external-id/db-01?source=cmdb" -d '{"name":"db-01","type":"server","address":"10.0.4.20"}'
```

Devices, sites, credentials, webhooks and alert routes support this. Run the same script after every change in your inventory.

## 13. Is the monitor itself healthy?

- Set `MONITOR_HEARTBEAT_URL` to a dead-man's-switch service (healthchecks.io or similar). The engine pings it every minute while its core loop is healthy, so you hear when the server, its database or its notifier stops.
- `docker compose logs monitor`, and `monitor health` for readiness.
- The engine also keeps its own health checks on a built-in `monitor` device in an internal platform tenant. A standalone tenant cannot see or route those; the heartbeat is the supported way to watch the server.

## Limits to know

| Limit | Default | Where it is set |
| --- | --- | --- |
| Devices / checks per tenant | 1000 / 10000 | `platform.tenant_defaults`, or the tenant record |
| Webhooks / routes per tenant | 20 / 50 | the same |
| Shortest check interval | 30 s | the same |
| API requests per minute per key | 1200 | the same |
| API body | 4 MiB | `api.max_body` |
| Push body, rate | 64 KiB, 2/s per token | `ingest.http` |

Beyond a limit, creating returns `409 limit-reached`, and the account also gets a `monitoring.tenant.*_limit_reached` event.
