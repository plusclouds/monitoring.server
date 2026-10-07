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
- **Not yet:** the tenant batch endpoint (`/ingest/v1/batch`, with F12), MQTT (part 2), SNMP traps and syslog (phase 2).

## Open questions

- The MQTT broker, topic structure and payload format are now taken from `fixleanplus.metric.collector` ([F12](F12-fixlean-collector-replacement.md)). Open questions specific to that migration are listed there.
