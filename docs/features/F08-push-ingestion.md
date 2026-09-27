# F08: Push ingestion — HTTP push, MQTT and last-seen

**Status:** Draft · **Phase:** MVP (HTTP push, MQTT, last-seen); phase 2 (SNMP traps, syslog) · **Related:** design sections 3 and 8

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
- Limits: 64 KB body, rate limit per token (default 1 request/s sustained, burst 10).

### MQTT

- The engine connects to one or more external brokers as a client (it does not run a broker). Broker connection, credentials and TLS are configured per tenant via `/v1/ingest/mqtt-brokers`.
- Mappings (`/v1/ingest/mqtt-mappings`): topic filter with named wildcards (`sensors/{site}/{device_id}/telemetry`), a device resolver (from a topic segment or payload field, matched to a device's `external_id`), payload format (`json`, `plain` number, and later `sparkplug-b`), and metric selectors as above.
- Unmapped messages are counted per topic prefix and visible in `GET /v1/ingest/mqtt-mappings/unmatched` to help onboarding.
- Subscriptions use QoS 1; shared subscriptions (`$share/monitor/...`) when the broker supports MQTT 5, so several ingest nodes can split load.

### Timestamps

Pushed timestamps are accepted if within ±5 minutes of server time; otherwise the server time is used and the check output notes the skew.

## API

`/v1/ingest/mqtt-brokers` (CRUD, `test`), `/v1/ingest/mqtt-mappings` (CRUD, `unmatched`), push checks through `/v1/checks` with `push.*` types, `POST /v1/checks/{id}/rotate-token`.

## Acceptance criteria

- A sensor publishing to Mosquitto every 60 s appears as metrics within 2 s of each message.
- Stopping the sensor opens an incident after 3 missed intervals, and it resolves on the next message.
- A leaked ingest token can be revoked without affecting any other device.
- 5,000 MQTT messages/s on one ingest node without message loss at QoS 1.

## Open questions

- **MQTT broker, topic structure and payload format** (open question in the design): take them from the existing project before building the mapping presets. The mapping model above is generic enough to start without them.
