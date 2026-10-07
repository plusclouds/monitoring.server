# Grafana for the monitoring engine

Starter dashboards and provisioning for a Grafana that reads the engine's
PostgreSQL database ([F07](../../docs/features/F07-metrics-store-and-retention.md),
[ADR-0003](../../docs/adr/0003-metrics-storage-layout.md)).

Grafana connects as the `monitor_grafana` role. That role can read only these
views, never the base tables. Each session is read-only and has a 30-second
statement timeout.

| View | One row per |
| --- | --- |
| `metrics_raw` | raw value: `time`, `series_id`, `tenant_id`, `device_id`, `check_id`, `object`, `name`, `unit`, `value` |
| `metrics_5m`, `metrics_1h` | rollup bucket: the same keys plus `min`, `max`, `avg`, `sum`, `count` |
| `metric_series_v` | series, with device and check names |
| `devices_v` | device, with `availability` and `open_incidents` |
| `check_states_v` | check, with its current state |
| `incidents_v` | incident, with device and check names and `duration` |

**The views show every tenant.** This Grafana is for the platform operator.
Do not give customers access to it. Tenant-facing graphs go through the API
(`GET /v1/metrics/query`), which enforces tenant isolation.

## Dashboards

- **Device overview:**
  - Devices by availability.
  - Open incidents.
  - The checks of the selected device and its metrics.
- **Website checks:** for HTTP checks:
  - Status.
  - Timing breakdown (DNS, TLS, time to first byte, total).
  - Status codes.
- **Network interfaces** (`snmp.interfaces`): interfaces not up, traffic in and out, errors and discards per interface, and every interface with its alias and speed.
- **Server hardware** (`redfish.health`): components not OK, temperatures, fans, power use and power supply output, and each system's vendor, model, serial, BIOS and BMC firmware.
- **Power and environment** (`snmp.pdu`, `snmp.ups`, `snmp.sensor`): devices not OK, PDU power, phase current, temperature, humidity, UPS charge and runtime, and door, leak and smoke contacts.
- **XCP-ng pools** (`xapi.pool`): hosts, host CPU and memory, VM CPU and disk IOPS, storage repository use, and VMs that are not running or not OK.
- **Cameras** (`camera.snapshot`, `rtsp.stream`): check status, frames per second, bitrate, brightness, sharpness against the reference picture, scene change and RTP loss.

The collector dashboards read `check_objects_v` (each object of a collector check with its state and labels) and name series by object through `metric_series_v.object_name`. Every dashboard query is run against the database by `TestGrafanaDashboards`.

Time-series panels choose a view from the panel interval:
- intervals under 5 minutes read `metrics_raw`;
- intervals under 1 hour read `metrics_5m`;
- longer intervals read `metrics_1h`.

So long ranges never scan raw samples. Rollups trail real time by a few minutes; the API's query endpoint fills that gap from raw data, but these dashboards do not.

## Development stack

```sh
docker compose -f deploy/docker-compose.yml --profile grafana up -d --wait
open http://127.0.0.1:3000    # admin / admin
```

## Production

1. Set `MONITOR_GRAFANA_PASSWORD` in `deploy/compose/.env`. On the next `docker compose up`, the `db-init` service lets `monitor_grafana` log in with that password and grants it the views.
2. Run Grafana wherever you like, with:
   - `provisioning/` mounted at `/etc/grafana/provisioning`;
   - `dashboards/` mounted at `/var/lib/grafana/dashboards/monitor`;
   - these environment variables:

   | Variable | Value |
   | --- | --- |
   | `MONITOR_GRAFANA_DB_HOST` | `host:port` of the database |
   | `MONITOR_GRAFANA_DB_SSLMODE` | `disable` inside a private Docker network, `verify-full` otherwise |
   | `MONITOR_GRAFANA_PASSWORD` | the role's password |

3. The database is on an internal network in the compose deployment. Run Grafana in the same project, or publish the database to Grafana's host only.
