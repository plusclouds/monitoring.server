# F12: Replacing the FixLean sensor collector

**Status:** Draft · **Phase:** MVP (ingest, sensor model, thresholds, events); migration runs after M5 · **Related:** [ADR-0013](../adr/0013-embedded-mqtt-broker.md), [F05](F05-state-and-incidents.md), [F08](F08-push-ingestion.md), [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)

## Summary

The engine takes over what `fixleanplus.metric.collector` does for sensors: it is the MQTT broker the devices connect to, registers sensors on first contact, discovers their telemetry fields, stores measurements, tracks online/offline, evaluates thresholds and emits alarm events. When the migration is done, the collector's MQTT side is switched off.

## What the collector does today

Summary of the current behavior, read from its source (Node 22, Express 5, Aedes, InfluxDB, STOMP/Artemis, BMS):

| Area | Current behavior |
| --- | --- |
| Broker | Aedes inside the Node process, plain TCP `0.0.0.0:1883`, one replica, no TLS |
| Auth | One shared username/password for every device (from environment); no ACLs, so any client can publish anywhere or subscribe to `#` |
| Identity | Device = MAC address, from topic segment 2; client ID is `<MAC>-…` and changes on every reconnect |
| Topic | `<vendor>/<MAC>/…`; segment 1 becomes the `vendor` tag, the rest is ignored |
| Payload | Flat JSON. **Status payload** if it has any of `Product`, `Version`, `RSSI`, `Local IP`, `Network MAC`, `Last Reset Reason`, `Runtime MS`, `Memory Info`, `ESP-IDF Version`, `Compile Date`, `Compile Time`, `Current Running Application`, `status`. Otherwise **telemetry** if it has any non-metadata value. Metadata keys: `unixtimestamp` (seconds), `sampling_rate`, `sample_size`, `measurement_buffer_size` |
| Telemetry keys | Vibration aggregates (`vrms_*`, `grms_*`, `peak_*`, `peak_to_peak_*`, `kurtosis_*`, `skewness_*`, `crest_*`, `clearance_*`), `temperature`, and whatever else a device sends; categorised as vibration / peaks / statistical / system / general |
| Registration | Unknown MAC creates a `fixlean-sensor` record in BMS, subject to a license check (`isPlus`, `totalSensor`) made on every message (cached 60 s) |
| Field discovery | New telemetry key adds a telemetry definition to the sensor record (name, display name, unit, category, chart and alert settings) |
| Storage | InfluxDB measurement `sensor_telemetry`, tags `macAddress`, `vendor`; only numeric fields |
| Online/offline | Connect marks online (stores keepalive); disconnect and keepalive timeout mark offline immediately; every 30 s, sensors silent longer than 1.5 × keepalive (clamped 60–300 s) go offline. "Recovered" is announced only when real data arrives, not on reconnect |
| Thresholds | Per telemetry: a list of rules with level (warning/critical), operators `gt`, `gte`, `lt`, `lte`, `eq`, `neq`, in range, out of range; unknown operator never fires; strict equality; 300 s cooldown per rule; records when the breach first started |
| Events | STOMP `/topic/utesk`: `sensor-threshold-triggered` (type 10), `sensor-threshold-resolved` (type 11), sensor silent, sensor recovered, throttled live telemetry ticks. Consumed by `fixlean.notification`, the alarm gateway and `fixlean.client` |
| Lake input | Subscribes to STOMP `/topic/utesk-lake-telemetry` (`{ macAddress, vendor, telemetries, unixtimestamp }`) from `fixleanplus.lake.collector`, which writes Influx itself; the collector only runs thresholds on it |
| Read side | Flux query API for Grafana widgets (`/api/widgets`, `/api/influx/meta`, `/api/datasources`), panel alert runner, scheduled dashboard reports |

## Scope

| In scope for the engine | Out of scope (decision needed, see open questions) |
| --- | --- |
| MQTT broker, device auth, topic and payload handling | Widget query API (Flux), datasource registry |
| Sensor auto-registration within tenant limits | Scheduled dashboard reports |
| Telemetry field discovery and metric metadata | Panel alert runner over arbitrary queries |
| Status payloads to inventory | Historical InfluxDB data (migrate or keep read-only) |
| Online/offline from connection events plus last-seen | |
| Thresholds with the same operators and levels | |
| Alarm events (webhooks) and live telemetry (SSE) | |
| Lake telemetry input | |
| Migration tooling from BMS sensor records | |

## Behavior in the engine

### Connection and tenant

- Devices connect to the embedded broker ([ADR-0013](../adr/0013-embedded-mqtt-broker.md)). The credential decides the tenant.
- **Legacy compatibility:** each current FixLean installation's shared username/password is registered as a `legacy` credential of one tenant, on the plain 1883 listener, so existing firmware works unchanged. Its ACL still keeps it inside its tenant.
- **Target state:** per-device credentials (username = MAC, generated password or client certificate), TLS on 8883, ACL `+/{mac}/#`. Delivered to devices by firmware/OTA update.

### Topic and payload profile

The FixLean behavior becomes a built-in **MQTT profile** `fixlean-esp`, one of the payload mappings in F08:

```yaml
profile: fixlean-esp
topic: "{vendor}/{device_key}/#"
device_key: mac            # matched to the device's MAC (stored as external ID, source "mac")
payload: json-flat
timestamp: { field: unixtimestamp, unit: s, fallback: receive_time }
metadata_fields: [unixtimestamp, sampling_rate, sample_size, measurement_buffer_size]
status_signature: [Product, Version, RSSI, "Local IP", "Network MAC", "Last Reset Reason",
                   "Runtime MS", "Memory Info", "ESP-IDF Version", "Compile Date",
                   "Compile Time", "Current Running Application", status]
labels: { vendor: "{vendor}" }
```

- A payload is **status** or **telemetry**, never both (a status signature wins), exactly as today, so nothing is recorded twice.
- **Status payloads** update device inventory: firmware version, product, ESP-IDF version, running application, compile date, last reset reason, local IP, network MAC. RSSI, runtime and memory values are also stored as metrics (`rssi_dbm`, `runtime_ms`, `memory_free_bytes`, `memory_min_free_bytes`), so signal strength and memory leaks can be graphed and alerted on.
- **Telemetry payloads:** every numeric non-metadata field is a metric. Non-numeric fields are ignored and counted.
- Series are keyed by device, never by MQTT client ID (the collector learned this the hard way: 59 client IDs for one MAC).

### Auto-registration

- The first message from an unknown MAC on a tenant credential creates a device: `type: sensor`, `name` = MAC, external ID `{source: mac, id: <MAC>}`, tag `vendor`, and a `push.mqtt` check using the `fixlean-esp` profile.
- The **tenant's `max_devices` limit** replaces the per-message license call. PlusClouds (or the FixLean license service through PlusClouds) sets the limit and status when provisioning the tenant ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)). Over the limit, messages are dropped and counted, the device appears in `GET /v1/ingest/unregistered`, and one `monitoring.tenant.device_limit_reached` event is sent.
- Auto-registration can be turned off per tenant (only pre-registered devices accepted).

### Field discovery and metric metadata

- A new telemetry key adds a metric definition to the device's push check: `key`, `display_name` (= key), `unit` (empty), `category` (same rules as today: vibration, peaks, statistical, system, general), `active`. It also bumps the check's metric layout version ([ADR-0003](../adr/0003-metrics-storage-layout.md)), so storage stays one row per message.
- Definitions are editable through the API (`PATCH /v1/checks/{id}/metrics/{key}`): display name, unit, category, active. Inactive keys are not stored.
- Discovered keys per device are capped (default 200) so that a misbehaving device cannot create unlimited series.

### Online/offline

- Every MQTT device gets an `mqtt.connection` check, flagged as its host check, fed by broker events:
  - connect → OK;
  - clean disconnect or keepalive timeout → CRITICAL with `failure_count: 1`. This matches today's immediate offline marking. Tenants can raise the count for flaky links.
- The `push.mqtt` last-seen rule remains the fallback, for devices with keepalive 0 or messages that stop while the TCP session stays up: timeout = 1.5 × keepalive, clamped to 60–300 s, as today.
- **Recovery needs data:** after an offline incident, the connection check returns to OK only when the first real message arrives, not on reconnect. Today's "sensor recovered" event means "data flows again", and we keep that meaning.
- Because the connection check is the host check, threshold incidents on an offline sensor are suppressed under the offline incident (dependency suppression, [F05](F05-state-and-incidents.md)).

### Thresholds

Current rules map onto the engine's threshold rules ([F05](F05-state-and-incidents.md)), which gain the operators FixLean needs:

| FixLean | Engine |
| --- | --- |
| `gt`, `gte`, `lt`, `lte` | `>`, `>=`, `<`, `<=` |
| `eq`, `neq` (strict) | `==`, `!=` (numeric, strict) |
| in range / out of range (`value`, `valueMax`) | `between`, `outside` with `value` and `value_max` |
| List of rules per telemetry, critical checked first | Multiple rules per metric; the worst matching level wins |
| Unknown operator never fires | Unknown operators are rejected when the rule is saved |
| 300 s cooldown per rule | Not needed: one incident per breach episode; reminders use the route's `repeat_interval` |
| `firstTriggeredAt` for duration-based alarm cards | Incident `opened_at` plus the rule's `for` duration |
| Rule ID and name in events | Rule `id` and `name` carried on the incident and in every event |

### Events

| Today (STOMP `/topic/utesk`) | Engine |
| --- | --- |
| `sensor-threshold-triggered` (10) | `monitoring.incident.opened` / `monitoring.incident.updated` for a threshold incident |
| `sensor-threshold-resolved` (11) | `monitoring.incident.resolved` |
| sensor silent | `monitoring.incident.opened` on the `mqtt.connection` check |
| sensor recovered | `monitoring.incident.resolved` on the `mqtt.connection` check |
| live telemetry tick (throttled) | SSE `GET /v1/events` event `monitoring.telemetry.tick`, throttled per device (default one per 5 s), only for subscribers that ask for it |

Every event carries the device's external IDs (MAC and the BMS `__dataId` if linked), so FixLean consumers can keep keying by them.

### Lake input

`fixleanplus.lake.collector` stops writing InfluxDB and announcing on STOMP. It sends its measurements to the engine's HTTP push endpoint instead, in batches (`POST /ingest/v1/batch` with `macAddress`, `vendor`, `telemetries`, `unixtimestamp`, and its extra labels `equipment`, `facility`, `sensorPlace`). The engine becomes the single place where measurements are stored and thresholds evaluated. This removes today's split, where the lake path writes storage but the collector evaluates thresholds.

A STOMP ingester plugin is a fallback only if the lake collector cannot be changed in time.

## Migration

| Step | What happens | Exit criterion |
| --- | --- | --- |
| 0. Inventory | Count installations, sensors, message rate, credentials; confirm firmware can be updated over the air | Numbers recorded in this spec |
| 1. Shadow | Engine runs in external-broker mode ([ADR-0013](../adr/0013-embedded-mqtt-broker.md)), subscribed to `#` on the current Aedes broker with the shared credentials, into a shadow tenant with notifications off | Two weeks with the same message counts and the same threshold events (compared with STOMP events) within 1 % |
| 2. Import | Script reads `fixlean-sensor` records from BMS and upserts devices (MAC and `__dataId` as external IDs), metric definitions and threshold rules through the API | Every BMS sensor has a device; spot-check rules per installation |
| 3. Cutover (per installation) | Legacy credential and 1883 listener enabled for the tenant; broker hostname moved to the engine's ingest load balancer; lake collector switched to HTTP push; webhooks to FixLean consumers enabled; collector's MQTT stopped but kept deployed for rollback | 48 h without missing sensors; alarms reach FixLean consumers |
| 4. Harden | OTA rollout of per-device credentials and TLS; legacy credential disabled when its connection count is zero for 7 days | No legacy connections for the tenant |
| 5. Decommission | Collector removed once the read-side question is settled | — |

Rollback during step 3 is moving the broker hostname back to the collector.

## API additions

`/v1/ingest/mqtt-credentials` (per-device and legacy credentials, CRUD, rotate), `/v1/ingest/profiles` (list built-in profiles such as `fixlean-esp`), `/v1/ingest/unregistered`, `/v1/checks/{id}/metrics` (metric definitions), `/v1/devices/{id}/connection` (current MQTT session: node, since, keepalive, client ID).

## Acceptance criteria

- An unmodified FixLean ESP32 sensor connects to the engine's legacy listener with its current credentials and appears as a device with inventory, metrics and the discovered telemetry keys.
- For the same recorded message stream, the engine opens and resolves the same threshold incidents as the collector emitted `sensor-threshold-triggered` and `sensor-threshold-resolved` events (replay test built from the collector's own test fixtures).
- Pulling a sensor's power opens one offline incident within keepalive × 1.5; threshold incidents on that sensor are suppressed.
- A device credential cannot publish under another MAC or subscribe to `#`.
- A tenant at its device limit does not gain devices, and existing devices keep reporting.

## Open questions

1. **Read side:** the widget query API, datasource registry, panel alerts and dashboard reports are UI features, which the engine does not have. Do they move to Grafana dashboards plus `/v1/metrics/query`, into another FixLean service, or stay in a slimmed-down collector reading the engine? This decides when step 5 can happen.
2. **Event consumers:** `fixlean.notification`, the alarm gateway and `fixlean.client` read STOMP today. Will they accept webhooks and SSE, or do we need a small webhook-to-STOMP bridge during the transition?
3. **Sensor metadata ownership:** after import, is the engine the source of truth for sensor names, units and thresholds, with FixLean screens editing them through our API? Or does BMS stay the source and sync to us? Proposed: the engine owns them; BMS keeps only the link.
4. **Historical InfluxDB data:** migrate it into PostgreSQL (a one-off export tool), or keep InfluxDB read-only until its retention expires?
5. **Tenant mapping:** does each FixLean installation correspond to one PlusClouds account?
6. **Firmware:** can all deployed sensors take an OTA update for per-device credentials and TLS? Any that cannot stay on the legacy listener indefinitely.
