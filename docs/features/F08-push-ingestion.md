# F08: Push ingestion — HTTP push, MQTT and last-seen

**Status:** Draft · **Phase:** MVP (HTTP push, MQTT, last-seen); phase 2 (SNMP traps, syslog) · **Related:** design sections 3 and 8, [ADR-0013](../adr/0013-embedded-mqtt-broker.md), [F12](F12-fixlean-collector-replacement.md)

## Summary

Many IoT devices push data instead of being polled. Ingesters receive pushed data, map it to devices and metrics, and feed the same engine as polled checks. For push devices, silence is the failure, so every push source has a last-seen check.

## Behavior

### Push sources as checks

A push source is a check with plugin type `push.http` or `push.mqtt`. It has no schedule; its config holds the mapping, and its `expected_interval` drives the last-seen rule: if no message arrives for `expected_interval × missed_count` (default 3), the check goes CRITICAL with output "no data since …". This reuses the state machine without special cases.

### HTTP push

- Separate listener (`ingest` role, own port), never on the management API.
- `POST /ingest/v1/{check_id}` with `Authorization: Bearer <ingest token>`. Each push check has its own token, revocable alone.
- Body: JSON. The mapping extracts metrics with JSONPath-like selectors: `{ "temperature": "$.sensors.temp", "humidity": "$.sensors.hum" }`, and optionally a status field.
- Batch form: an array of objects with timestamps, for devices that buffer.
- **Tenant batch endpoint** for gateways and other collectors that report for many devices (for example `fixleanplus.lake.collector`): `POST /ingest/v1/batch` with a tenant-scoped ingest token. Each item names its device by external ID (`{source, id}`, such as `{mac, AA:BB:…}`) and carries metrics, labels and a timestamp. Auto-registration rules are the same as for MQTT ([F12](F12-fixlean-collector-replacement.md)).
- Limits: 64 KB body, rate limit per token (default 1 request/s sustained, burst 10).

### MQTT

Two modes, chosen per tenant ([ADR-0013](../adr/0013-embedded-mqtt-broker.md)):

- **Embedded broker (default).** Devices connect to the engine's `ingest` role directly: TLS on 8883, per-device credentials and topic ACLs, and a legacy plain listener on 1883 only for migrating tenants. The credential decides the tenant. Connect, disconnect and keepalive-timeout events feed an `mqtt.connection` check per device, so offline detection does not wait for last-seen timers.
- **External broker.** The engine connects as a client to a broker the tenant already runs. Broker connection, credentials and TLS are configured via `/v1/ingest/mqtt-brokers`. Subscriptions use QoS 1, with shared subscriptions (`$share/monitor/...`) when the broker supports MQTT 5, so several ingest nodes can split load. There are no connection events in this mode; offline detection uses last-seen only.

Both modes use the same **mappings** (`/v1/ingest/mqtt-mappings`):

- topic filter with named wildcards (`{vendor}/{device_key}/#`, `sensors/{site}/{device_id}/telemetry`);
- a device resolver (from a topic segment or payload field, matched to a device's external ID);
- payload format (`json-flat`, `json` with selectors, `plain` number, and later `sparkplug-b`);
- timestamp field and unit, metadata fields to ignore, and a status signature that routes a payload to inventory instead of metrics;
- metric selectors as above, or "every numeric field" with field discovery.

Built-in **profiles** bundle a full mapping; the first is `fixlean-esp` for the FixLean ESP32 sensors ([F12](F12-fixlean-collector-replacement.md)).

Unmapped messages are counted per topic prefix and visible in `GET /v1/ingest/mqtt-mappings/unmatched` to help onboarding. Messages from unknown devices are handled by the tenant's auto-registration setting.

### Timestamps

Pushed timestamps are accepted from up to 24 h in the past (devices that buffer while offline) to 5 minutes in the future. Past data older than 2× the expected interval updates metrics but not state (same rule as late probe results). Timestamps outside that window are replaced with the server time, and the check output notes the skew.

## API

`/v1/ingest/mqtt-credentials` (CRUD, rotate), `/v1/ingest/mqtt-brokers` (CRUD, `test`), `/v1/ingest/mqtt-mappings` (CRUD, `unmatched`), `/v1/ingest/profiles`, `/v1/ingest/unregistered`, `/v1/ingest/tokens` (tenant batch tokens), push checks through `/v1/checks` with `push.*` types, `POST /v1/checks/{id}/rotate-token`.

## Acceptance criteria

- A sensor publishing to the embedded broker (and, in external mode, to Mosquitto) every 60 s appears as metrics within 2 s of each message.
- A sensor losing power opens an offline incident from the keepalive timeout, without waiting for 3 missed intervals.
- Stopping the sensor opens an incident after 3 missed intervals, and it resolves on the next message.
- A leaked ingest token can be revoked without affecting any other device.
- 5,000 MQTT messages/s on one ingest node without message loss at QoS 1.
- The HTTP push body parser, the MQTT payload profiles and mapping selectors, and (phase 2) the SNMP trap and syslog parsers have Go fuzz tests run in CI; no input crashes the ingest role or exceeds its memory limits.

## As built (M5, part 1: `v0.9.0`)

- **`push.http`** (plugin kind `ingester`, billing class `push`). Config: `metrics` (name to selector), `units`, `status`, `output`, `timestamp`, `missed_count` (default 3). Selectors are a JSONPath subset without wildcards: `$.a.b`, `$.list[0].v`, `$["name with-dash"]`. Metric values may be numbers, numeric strings or booleans (1/0). Status values: `ok`/`warning`/`critical`/`unknown`, `0`..`3`, `true`/`false`, plus `up`, `down`, `warn`, `crit`, `error`. Without `status` every message is OK and thresholds decide. A message without its timestamp gets the arrival time.
- **Per-check metric layout.** An ingester's metrics come from the check's config (`plugin.Ingester.MetricsFor`), in alphabetical order; thresholds are validated against those names. Renaming metrics starts new series.
- **Token.** Created with the check and returned once as `push_token` (`mpush_<8>_<43>`); only its SHA-256 is stored. `GET /v1/checks/{id}` shows `push` (`ingest_path`, `ingest_url` from `ingest.http.public_url`, `token_prefix`, `token_created_at`, `last_push_at`). `POST /v1/checks/{id}/rotate-token` replaces it; the old token stops at once.
- **Listener.** `POST /ingest/v1/{check_id}` on the `ingest` role (`ingest.http.listen`, behind the reverse proxy at `<public URL>/ingest/`). 202 `{"accepted": n}`; 401 for a missing or wrong token (the same answer for an unknown check), 400 for a malformed message, 409 for a disabled check, 413 above `max_body`, 429 above the per-check rate. A body is one object or an array of up to 1,000 (oldest first after sorting). The ingest role must run in the engine's process (results pass through the same channel as the runner's).
- **Last seen.** Every 5 s the listener claims, in one `UPDATE … RETURNING`, the enabled push checks silent for `interval × missed_count` and sends each a CRITICAL "no data since …" result, at most once per interval while the silence lasts; several ingest nodes never report twice. The next message resolves it. Push checks default to `failure_count` 1, since `missed_count` already waits.
- **Timestamps.** Up to 24 h old or 5 minutes ahead are kept; outside that window the server time is used and the output says so. Results older than 2 × the interval are metrics only (the engine's late rule).
- **Fuzzing.** `FuzzParse` runs for 15 s in `make test` (CI).
- **Billing.** `push.http` uses `default_weight` (1) until PlusClouds sets a weight.
- **Not yet:** the tenant batch endpoint (`/ingest/v1/batch`, with F12), SNMP traps and syslog (phase 2).

## As built (M5, part 2: `v0.10.0`)

- **Broker.** `mochi-mqtt/server` v2.7.9 (MIT) in the `ingest` role: MQTT 3.1.1 and 5, TLS listener (`ingest.mqtt.listen`, 8883) and the plain listener (`ingest.mqtt.legacy`, 1883) when enabled. The certificate is read again when its file changes (checked once a minute), for Let's Encrypt renewals. QoS up to 1; every message is acknowledged after the pipeline took it and never routed or retained; nothing may subscribe. Clean sessions only.
- **Credentials** (`/v1/ingest/mqtt-credentials`): username (global), Argon2id password (given or generated, shown once; `rotate`), `kind` `device` (bound to one `device_key`) or `shared` (any key of the tenant, as FixLean firmware uses today), `profile`, `allow_plain` (only such credentials may use 1883), `auto_register`, `enabled`. Successful logins are cached for `credential_cache_ttl` (60 s), so a disabled or rotated credential stops new connections within it; `max_concurrent_auth` bounds Argon2 work; connect rate per IP and publish rate per client are enforced.
- **Topics.** `<prefix>/<device_key>/...`: the second segment is the device key (a MAC, a serial), compared without case and stored upper-case. A device credential publishing under another key is disconnected (MQTT 3.1.1) or gets "not authorized" (MQTT 5).
- **Devices.** The first message under an unknown key creates a `sensor` device named after the key (tag `vendor` from the first segment) with a `push.mqtt` data check and an `mqtt.connection` check (the host check unless the device has one); within `max_devices`. Auto-registration off or the tenant at its limit: the message is acknowledged, dropped and listed in `GET /v1/ingest/unregistered`. `POST /v1/devices/{id}/mqtt` pre-registers a device (`GET` shows the binding and the live session, `DELETE` unbinds). MQTT checks do not count toward `max_checks`; `push.mqtt` and `mqtt.connection` cannot be created through `POST …/checks`.
- **Profiles** (`GET /v1/ingest/profiles`): `fixlean-esp` (see F12) and `json`: every numeric field (booleans as 1/0, nested objects joined with `_`, three levels) becomes a metric named in lowercase with `_`; or the check's `metrics` selectors pick them as for `push.http`. New keys are added to the check's `fields` (sorted) up to `max_discovered_fields` (200), which changes the layout version.
- **Online/offline.** A session is tied to the device of the first key it publishes under (a client publishing for several keys is a gateway and reports no connection). The first data message of a session sets the connection check OK; a disconnect or keepalive timeout (1.5 × keepalive) sets it CRITICAL, unless the node is shutting down or a newer session took over (`mqtt_sessions` row check). The `push.mqtt` last-seen rule is the fallback: interval 1.5 × keepalive clamped to 60–300 s, `missed_count` 1. Last-seen times are written every 2 s in one statement.
- **Billing.** `mqtt.connection` weight 0, `push.mqtt` the default weight 1 (confirmed by PlusClouds, 2026-10-07): one sensor bills once.
- **Tests.** `TestMQTTBroker` (credentials, listeners, registration, discovery, inventory, thresholds, disconnect, ACL, limits, pre-registration, rotation), `FuzzDecode` in CI.
- **Not yet:** external-broker mode (shadow migration, F12 step 1), PROXY protocol, the metric metadata API (`PATCH /v1/checks/{id}/metrics/{key}`), SSE telemetry ticks, client certificates.

## As built: PlusClouds VM agent (`v0.12.0`)

- **Source.** The PlusClouds VM agent (`github.com/plusclouds/ubuntu-agent`, Linux and Windows) publishes `system.SystemMetrics` every 30 s on the platform's NATS, subject `vm.<uuid>.telemetry` (also kept 15 minutes in the `VM_TELEMETRY` stream). The ingest role subscribes to `ingest.nats.subject` (`vm.*.telemetry`) in the queue group `ingest.nats.queue`, so ingest nodes share the messages. No agent change is needed.
- **Check.** `vm.agent` (ingester with objects), config `{"vm_uuid": "<uuid>"}`, unique on the server (409 otherwise). Monitoring is **opt-in per VM** (decided 2026-10-07): when a customer enables monitoring for a VM, the panel creates the device (external ID = the VM UUID) and its `vm.agent` check in the customer's tenant; disabling deletes them. The panel must check that the VM belongs to that customer before creating the check: the engine cannot, and the first tenant to claim a VM UUID receives its telemetry. A message is routed by the UUID in its subject; one whose payload names another VM is dropped, as are messages for VMs without a check (counted in `nats_ingest_messages_total`, outcome `no-check`).
- **Objects.** `host` (CPU usage, cores, load averages, memory), `disk:<mountpoint>` (usage, size, read/write rates, IOPS, busy %) and `net:<interface>` (up, receive/send bit/s from the counters' difference; a counter that went back, after a reboot, gives no rate). A down interface is a WARNING object. Thresholds with `object` (`*` or a key) apply as for collectors; `Manifest.Objects` lets an ingester carry objects.
- **Silence.** The push checks' last-seen rule: no telemetry for `interval × missed_count` (60 s × 3 by default) is CRITICAL, so a VM that is off or whose agent stopped shows as down when the check is its host check.
- **Connection.** `ingest.nats.url` (`nats://`, `tls://` or `wss://`), authenticated with `user` and `password_file`/`password`, `token_file` or a `.creds` file; reconnects for ever. The platform has to provide a user allowed to subscribe to `vm.*.telemetry`.
- **Billing.** Weight 2 per monitored VM (`vm.agent`, confirmed by PlusClouds on 2026-10-07, from `v0.12.1`); objects (disks, interfaces) are not billed.
- **Not yet:** service states (the agent sends none in telemetry; a watch list in the agent is planned), heartbeats on `agent.vm.<uuid>.evt`.

## As built: box.agent (`v0.13.0`)

- **Source.** `box.agent` (Go, on servers that run vLLM) POSTs a JSON heartbeat about once a second to `POST /ingest/v1/{check_id}` with `Authorization: Bearer mpush_…`. No NATS: it is an independent service. The token is minted for monitoring alone (not the token box.agent uses with llmocean.api).
- **Check.** `box.agent`, config `{"box_id": "<id>", "missed_count": 5}`, unique on the server (409). The provisioner (llmocean.api) creates the device (external `{source: "greenference", type: "llmbox", id: <box ID>}`) and the check with `interval_seconds: 1`, receives `push_token` once, and gives the box `-monitor-url`/`-monitor-token`. `POST /v1/checks/{id}/rotate-token` rotates it; disabling or deleting the check on deprovision stops the silence alert. The tenant's `min_check_interval_seconds` must be 1 or less.
- **Objects.** `host` (load, memory, disk, OOM kills, router connection, the box's own count of this server's answers by class), `vllm` (up, health, restarts, queue) and `gpu:<idx>` (temperature, utilisation, memory, ECC, last XID, throttle). A missing GPU array is unknown, not failed.
- **State.** XID 79, 48 or 64, or uncorrectable ECC, is CRITICAL at once. `booting`, `downloading`, `restarting` and `stopped` are never incidents. Otherwise the agent's `severity` (ok, degraded, failed) maps to OK, WARNING, CRITICAL; the server decides what happens next. Silence for `missed_count` intervals (5) is CRITICAL through the last-seen rule (swept every 5 s).
- **Counters.** `ingest_http_responses_total{code}` counts every answer of the push endpoint by status code.
- **Billing.** Weight 2 per box is a placeholder until PlusClouds confirms it.
- **Not yet:** the alert route to the llmocean.api webhook and the provisioning calls are llmocean.api's side.

## Open questions

- The MQTT broker, topic structure and payload format are now taken from `fixleanplus.metric.collector` ([F12](F12-fixlean-collector-replacement.md)). Open questions specific to that migration are listed there.
