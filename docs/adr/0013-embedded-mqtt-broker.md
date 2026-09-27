# ADR-0013: Embedded MQTT broker in the ingest role

**Status:** Proposed · **Date:** 2026-09-27

## Context

The engine will replace `fixleanplus.metric.collector` ([F12](../features/F12-fixlean-collector-replacement.md)). Today that service *is* the MQTT broker: it runs Aedes inside its Node process on plain TCP 1883, and ESP32 sensors connect to it directly. It relies on things only a broker sees:

- **connect** — marks the sensor online and records its keepalive;
- **disconnect** and **keepalive timeout** — mark it offline immediately, without waiting for a last-seen timer;
- **authentication** — decides who may connect.

We are free to choose how MQTT works in the engine. Devices only publish; nothing in the current system publishes to devices.

Requirements:

- keep the connection events above;
- per-device credentials and topic ACLs (today everyone shares one password and can read every topic);
- TLS;
- scale to the design target without a single-replica bottleneck;
- no extra infrastructure if we can avoid it ([ADR-0002](0002-postgresql-as-the-only-infrastructure-dependency.md));
- permissive licenses ([ADR-0010](0010-dependency-license-policy.md));
- a compatibility path for devices already in the field.

## Options considered

1. **Embedded broker in the `ingest` role**, using [`mochi-mqtt/server`](https://github.com/mochi-mqtt/server) (Go, MIT, MQTT 3.1.1 and 5). The engine gets connect, disconnect, keepalive, authentication, ACL and publish hooks in-process, like the current Aedes setup, but in Go and backed by our database.
2. **External broker** (Mosquitto, EMQX, NanoMQ, VerneMQ) with the engine subscribing as a client.
   - Full-featured and battle-tested.
   - Another stateful service to run, secure and monitor.
   - Connection events arrive only through broker-specific mechanisms (`$SYS` topics, webhooks, plugins), so online/offline handling becomes broker-specific.
   - Per-device auth needs a broker plugin that queries our database.
   - Licenses need checking per product and version (EMQX's open-source licensing has changed in recent releases).
3. **Keep the Node collector as a front end** and forward to the engine. Keeps the shared-password and single-replica problems, and keeps a second codebase alive. Rejected.

## Decision

Option 1 as the default, keeping F08's **external-broker client mode** as a second ingest mode for tenants that already run their own broker.

### Embedded broker

| Aspect | Decision |
| --- | --- |
| Library | `mochi-mqtt/server` v2 (verify license and maintenance status at adoption, per ADR-0010) |
| Protocols | MQTT 3.1.1 and 5.0 |
| Listeners | `mqtts` on 8883 (TLS, default); legacy plain `mqtt` on 1883, off by default and enabled only for migrating tenants ([F12](../features/F12-fixlean-collector-replacement.md)); WebSocket later if a device needs it |
| Authentication | Hook queries `mqtt_credentials` (cached): per-device username/password (stored as Argon2id hash) or client certificate. Legacy shared credentials map to one tenant and are flagged `legacy` |
| Authorization (ACL) | A device credential may only publish under its own topic prefix (for example `+/{mac}/#`) and may not subscribe to anything unless configured. Legacy credentials are limited to `+/+/#` inside their tenant |
| Tenant resolution | From the credential, never from the topic. The same MAC under two tenants is two devices |
| Connection events | `OnConnect`, `OnDisconnect` and keepalive expiry feed the engine as connection results for the device ([F12](../features/F12-fixlean-collector-replacement.md)) |
| Publish handling | `OnPublish` hook hands the message to the ingest pipeline and does **not** retain or route it: there are no other subscribers. QoS 1 acknowledgement is sent after the message is accepted into the pipeline's bounded buffer |
| Session state | In memory on the node. No persistence: devices reconnect with clean sessions |
| Limits | Max connections per node, max packet size (default 64 KB), per-client publish rate, connect rate per source IP |

### Scaling

Devices only publish and the engine is the only consumer, so brokers on different nodes never need to route messages between each other. Several `ingest` nodes sit behind a TCP (layer 4) load balancer; each device is connected to exactly one of them at a time.

- Messages from one device are processed in order because they arrive over one connection and the pipeline keys its per-device queue by device ID.
- Which node holds a device's connection is recorded in `mqtt_sessions (device_id, node_id, client_id, connected_at, keepalive)`. This lets any node answer "is it connected?" and, later, route commands to devices (a `NOTIFY` to the owning node) if downlink is ever needed.
- When a device reconnects to another node, the new session row replaces the old. A late disconnect from the old node is ignored because its `client_id` or `connected_at` no longer matches.

### External-broker mode

This keeps the F08 design: the engine subscribes as a client, preferably with MQTT 5 shared subscriptions. Connection events are not available in this mode; online/offline falls back to last-seen timing. This mode is also what we use to run the engine in **shadow mode** against the current Aedes broker during migration ([F12](../features/F12-fixlean-collector-replacement.md)).

## Consequences

- No new infrastructure: `monitor serve --roles ingest` is the broker.
- We operate an internet-facing MQTT server. The listener needs the same care as the API: TLS, rate limits, connection caps, and the self-metrics and alerts in [F11](../features/F11-self-monitoring.md) (connections, auth failures, publish rate, dropped messages).
- Broker features we don't need are not provided: bridging, retained-message fan-out, many subscribers per topic. A tenant who needs those uses external-broker mode.
- If `mochi-mqtt` turns out unsuitable (maintenance, performance), the broker sits behind a small internal interface (`Authenticate`, `Authorize`, `OnConnect`, `OnDisconnect`, `OnPublish`), so it can be swapped for another embeddable broker or an external one without touching the pipeline.
- **Load-test gate:** 10,000 concurrent TLS connections on one node publishing every 10 s, with p99 publish-to-state latency under 1 s.
