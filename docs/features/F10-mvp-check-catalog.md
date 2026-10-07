# F10: MVP check and collector catalog

**Status:** Draft · **Phase:** MVP · **Related:** [F03](F03-plugin-sdk.md), design section 3, [ADR-0010](../adr/0010-dependency-license-policy.md)

## Summary

The MVP covers every requested target type at a basic level. This spec lists each built-in plugin, what it measures, and the implementation notes that matter. Plugin type names are stable identifiers used in the API.

## Availability checks

| Type | Library | Metrics | Status rules and notes |
| --- | --- | --- | --- |
| `icmp` | `pro-bing` | `rtt_avg_ms`, `rtt_max_ms`, `packet_loss_percent` | CRITICAL at 100 % loss. Default 3 packets, 200 ms apart. Usually flagged as the device's host check. IPv4 and IPv6. |
| `tcp` | standard library | `connect_ms` | CRITICAL on refused or timeout. Optional send/expect string for banners. |
| `udp` | standard library | `rtt_ms` | Needs a request payload and expected response; plain UDP "open" cannot be detected reliably. |
| `http` | standard library | `dns_ms`, `connect_ms`, `tls_ms`, `ttfb_ms`, `total_ms`, `status_code`, `body_bytes` | Method, headers, body, expected status set, keyword present/absent, redirect policy, source IP. Timing via `httptrace`. SSRF guard from tenant `allowed_target_networks`. |
| `tls` | standard library | `cert_days_left` | Chain validation, hostname match, expiry thresholds (defaults suggested at 21/7 days, set per check), SNI. Works on any TCP port, including STARTTLS for SMTP. |
| `dns` | `miekg/dns` | `query_ms` | Record type, server, expected answers (exact or contains), DNSSEC validation flag. |
| `whois` | RDAP over HTTP first, WHOIS fallback | `domain_days_left` | Default interval 12 h; min interval 1 h to respect registry limits. |

## SNMP

| Type | Metrics | Notes |
| --- | --- | --- |
| `snmp.system` | `uptime_seconds`, `cpu_percent`, `memory_used_percent` | CPU/memory OIDs from a vendor profile table (HOST-RESOURCES-MIB, Cisco, Juniper, MikroTik, Fortinet, HPE/Aruba to start). Reboot detected from `sysUpTime` going backwards; emits an inventory change event. |
| `snmp.interfaces` (collector) | per interface: `in_bps`, `out_bps`, `in_errors_rate`, `out_errors_rate`, `in_discards_rate`, `out_discards_rate`, `oper_status`, `speed_bps` | Uses `ifXTable` 64-bit HC counters with GETBULK; falls back to 32-bit only when HC is missing. Interfaces become child objects keyed by `ifIndex` plus `ifName` (re-mapped when `ifIndex` changes after reboot). Interface filters (by name regex, type, admin status) to skip unused ports. |
| `snmp.get` | `value` (gauge) or `rate` (counter) | One OID per check, with gauge or counter semantics, a scale factor and an optional expected value (`equals`, `contains`, `regex`); the escape hatch for devices without a profile. A plugin has a fixed metric layout, so several OIDs are several checks. |

Counter handling for every counter-based metric (design section 9):

- Rate = delta / elapsed seconds, using the collection timestamps.
- Negative delta: if `sysUpTime` went backwards, it is a reset — discard this sample. Otherwise, for 32-bit counters, assume one wrap if the resulting rate is plausible (below interface speed), else discard.
- Rates above interface speed × 1.1 are discarded as glitches.

Defaults: SNMPv3 `authPriv` (SHA-256/AES-128 or stronger where supported); v2c only when set explicitly per credential. Timeout 5 s for v3 (engine discovery costs a round trip), 2 retries; table walks run in the slow pool with minimum interval 60 s.

As built (M4):

- Library `gosnmp/gosnmp` (BSD-2-Clause). Every SNMP plugin takes one credential in the `auth` role, `snmp_v3` or `snmp_v2c`. The `snmp_v3` privacy protocols include `AES-192-C` and `AES-256-C` (draft-reeder, as Cisco implements them) next to `AES-192` and `AES-256` (draft-blumenthal, as Net-SNMP does).
- `snmp.system` profiles: `host-resources`, `net-snmp` (memory from UCD-SNMP-MIB, because hrStorage counts the page cache as used), `cisco`, `juniper`, `mikrotik` (HOST-RESOURCES), `fortinet` and `hpe` (ArubaOS-Switch; Aruba CX uses HOST-RESOURCES). `auto` picks by the longest matching `sysObjectID` prefix; a vendor profile whose objects are missing falls back to HOST-RESOURCES. CPU is the average over processors for HOST-RESOURCES and the busiest module for vendor tables. Uptime is `hrSystemUptime`, else `sysUpTime`. A restart (uptime going backwards, not the 497-day TimeTicks wrap) is reported in the output; the inventory change event waits for collectors.
- Results: no answer is CRITICAL (an agent also stays silent on a wrong v2c community); an SNMPv3 authentication failure, a missing object or a refused address is UNKNOWN.
- `snmp.ups` status: battery low or depleted and output off are CRITICAL; on battery, on bypass and battery replacement are WARNING. Thresholds on runtime and load are set on the check.
- `snmp.interfaces` (collector) reads `ifTable` and `ifXTable` with GETBULK, several columns per request. Objects are keyed by `ifName` (else `ifDescr`; `ifIndex.N` when names repeat), so renumbering after a reboot keeps history. Labels: `if_index`, `if_type`, `description`, `alias`, `speed`. Filters: `include` and `exclude` (regular expressions on name or description), `types` (default: all but software loopback), `admin_up_only` (default true), `max_interfaces` (1,000). An interface that is administratively up and operationally down (down, not present, lower layer down) is `down_status` (default CRITICAL); dormant is OK. Speed is `ifHighSpeed` when set. Counter rates as above, with the interface speed as the plausibility limit for traffic. Slow pool, minimum interval 60 s. One collector is one billed check.
- Tests run against snmpsim with recorded fixtures for Net-SNMP, Cisco, FortiGate, a UPS-MIB UPS, an APC UPS and an access switch, over v2c and SNMPv3 `authPriv` (SHA-256, AES).

## Server hardware

| Type | Library | What it checks |
| --- | --- | --- |
| `redfish.health` (collector) | `gofish` | Chassis thermal (temperatures with vendor thresholds, fans), power (PSU status, power consumption), system (overall health, CPUs, DIMMs), storage (controllers, physical and virtual disks, controller battery). Each component becomes a child object with its own status. Inventory: vendor, model, serial, BIOS/BMC firmware. Uses sessions (`X-Auth-Token`) instead of basic auth per request to reduce BMC load; logs out after each run. Minimum interval 60 s; BMCs are slow. |
| `ipmi.sensors` (collector) | IPMI library to be selected ([ADR-0010](../adr/0010-dependency-license-policy.md)) | Sensor data records: temperatures, fans, voltages, PSU state. Fallback for BMCs without usable Redfish. IPMI over LAN v2.0 (RMCP+) only. |

SEL/event log reading is phase 2.

As built (M4), `redfish.health`:

- A small Redfish client of our own (read-only GETs over a session), not `gofish`: monitoring needs a login, reads and a logout, and owning the client keeps the network policy, timeouts and vendor quirks in our hands. Login opens a session (`X-Auth-Token`), logged out after every run even when the run timed out; a BMC without a session service gets basic authentication.
- Credential: `redfish` in the `auth` role; use a read-only BMC account. HTTPS only. The BMC's certificate is not verified by default, because most BMCs ship a self-signed one (decided 2026-10-07); `verify_certificate: true` turns verification on, and an untrusted certificate is then UNKNOWN with the reason.
- Objects (keys `kind:id`; with several systems or chassis, `kind:scope:id`): `system` (labels vendor, model, serial, BIOS, power state, BMC firmware), `cpu`, `memory` (populated DIMMs only), `storage` (controllers), `drive`, `volume`, `temperature`, `fan`, `psu` and `power` (consumed watts). Absent and disabled components (empty slots) are not objects. `skip` drops kinds.
- Status per object is the BMC's own health (OK, Warning, Critical). A temperature without health is judged by the BMC's own upper thresholds. Metrics per object: `health` (0, 1, 2), `temperature_celsius`, `fan_rpm` or `fan_percent`, `power_watts`; thresholds on them are set on the check, per object if needed.
- Collections are read member by member, two at a time; members a BMC already expands are used as they are. Minimum interval 60 s (default 2 min), slow pool, one run per BMC at a time.
- Uses the `Thermal` and `Power` resources, which current Dell iDRAC 9, HPE iLO 5/6 and AMI MegaRAC (ASUS ASMB) BMCs still serve. Their successors (`ThermalSubsystem`, `PowerSubsystem`) wait until a BMC we run drops the old ones.
- Tested against the DMTF Redfish mockup server over HTTPS with `testdata/dell-r740`: the DMTF rackmount sample trimmed and made into a Dell PowerEdge with a PSU in warning, a failed DIMM, an empty slot, a failed disk and a degraded volume.
- Not yet verified on real iDRAC, iLO and ASUS BMCs (Gate 1 compatibility table).

## Cameras

| Type | Library | What it checks |
| --- | --- | --- |
| `rtsp.stream` | `gortsplib` | `DESCRIBE`, `SETUP`, `PLAY`; reads the stream for a sample window (default 5 s). Metrics: `fps`, `bitrate_kbps`, `width`, `height`, `rtp_loss_percent`, `jitter_ms`. CRITICAL if no frames arrive; WARNING if FPS or resolution differ from expected values. Codec from SDP (H.264/H.265). Does not decode video, so CPU cost stays low; frozen-image detection (identical frames) is phase 3. |

ONVIF device info and snapshots are phase 2 (with discovery).

As built (M4): `rtsp.stream` with `gortsplib` v5 (MIT; the v4 line is deprecated) and `mediacommon` for parameter sets.

- **Stream.** `vendor: auto` (default) tries the Hikvision (`/Streaming/Channels/<channel>01`), Dahua (`/cam/realmonitor?channel=<n>&subtype=0`) and Axis (`/axis-media/media.amp`) paths and remembers the one that answered; or a fixed `vendor`, or `path`. `substream` reads the second stream. Port 554, `tls` for RTSPS (certificate not verified), `transport` tcp (default, passes NAT) or udp (shows network loss). The camera account (`rtsp` credential) goes into the URL; the library answers basic and digest challenges.
- **Measures** over `sample_seconds` (default 5) without decoding video: frames (H.264/H.265 access units, or marked packets for other codecs), bitrate, resolution from the SDP's or the stream's SPS, RTP loss and jitter from the session statistics, time to the first frame. Metrics `fps`, `bitrate_kbps`, `width`, `height`, `rtp_loss_percent`, `jitter_ms`, `first_frame_ms` (`fps` is the Whoopsy! default).
- **Status.** No frames in the window is CRITICAL (fps 0). WARNING below `min_fps`, on a resolution other than `width`×`height`, or above `max_loss_percent` (5). A camera whose RTSP service does not answer is CRITICAL; a rejected credential, no stream at the paths or a refused address is UNKNOWN. Slow pool, one run per camera at a time, minimum interval 60 s.
- **Tests** run against an in-process `gortsplib` server streaming H.264 with a real 1280×720 SPS behind credentials.

As built (M4, requested 2026-10-07): `camera.snapshot`, picture checks from a JPEG snapshot, analysed in pure Go (no video decoder, no new dependency). The brands we run: Hikvision, Dahua, Axis, and ONVIF for the rest.

- **Source.** `vendor: auto` (default) tries Hikvision (`/ISAPI/Streaming/channels/<channel>01/picture`), Dahua (`/cgi-bin/snapshot.cgi?channel=<n>`), Axis (`/axis-cgi/jpg/image.cgi`) and ONVIF (GetCapabilities, GetProfiles, GetSnapshotUri with a WS-Security password digest), and remembers what answered; or a fixed `vendor`, or `snapshot_url` (a path or an http(s) URL without credentials). HTTP by default (`https`, `port`, `verify_certificate`); basic and digest authentication (MD5 or SHA-256) with the `rtsp` (camera account) or `http_basic` credential.
- **Measures**, on a grayscale copy at most 320 pixels wide: brightness (mean luma), contrast (its standard deviation), sharpness (variance of the Laplacian divided by the luma variance, so lighting changes do not read as blur), a 64-bit difference hash of the scene, and a 64×48 thumbnail.
- **Checks** (`checks` selects them; all by default):
  - covered: contrast below `covered_below` (4) is CRITICAL: a covered or sprayed lens, or a black picture;
  - brightness: below `dark_below` (10 %) or above `bright_above` (95 %) is WARNING;
  - frozen: the same picture (thumbnail difference under 0.3) `frozen_runs` (3) times in a row is CRITICAL;
  - blur: sharpness below `blur_below` (40 %) of the reference picture's is WARNING;
  - moved: the scene hash differing more than `moved_above` (25 %) from the reference is WARNING.
- **Reference picture.** Learned from the first normal picture (not covered, dark or overexposed); changing `reference_id` learns a new one (after re-aiming a camera). Blur and movement are not judged while the picture is dark or overexposed, so a night-time infrared image does not read as blurred or moved. The reference lives in the plugin state, which is kept in memory: after an engine restart it is learned again from the next normal picture.
- **Results.** A camera that does not answer is CRITICAL; a rejected credential, no working snapshot URL, a non-picture answer or a refused address is UNKNOWN. Metrics: `brightness_percent`, `contrast`, `sharpness`, `sharpness_percent_of_reference`, `scene_change_percent`, `frozen_runs`, `width`, `height`, `image_bytes`, `fetch_ms` (also the Whoopsy! default). Default interval 5 minutes, minimum 30 s.
- **Tests** use generated pictures (sharp, blurred, dark, overexposed, covered, black, another scene, the same picture repeated) from a fake camera with digest authentication, the vendor paths and an ONVIF service. Thresholds should be tuned on our own cameras before Gate 1.

## Hypervisors

| Type | What it collects |
| --- | --- |
| `xapi.pool` (collector) | Connects to the pool master via XAPI (JSON-RPC). Inventory: hosts, VMs (by UUID), storage repositories, networks; creates and updates child devices; re-parents VMs on migration. Host state: enabled, maintenance, pool master, HA. SR: size, utilization. VM: power state, guest tools, snapshot count and oldest snapshot age. |
| `xapi.rrd` (collector) | Per host, polls `/rrd_updates?start=<last>&host=true&cf=AVERAGE` (one call returns every host and VM metric changed since the timestamp, as the design recommends). Maps RRD data sources to metrics: host CPU per core and average, memory, PIF throughput/errors; VM CPU, memory target/actual, VBD IOPS/throughput/latency, VIF rx/tx/errors. Runs every 60 s by default. |

Credentials: a read-only XAPI user (`read-only` RBAC role in XenServer/XCP-ng). If the pool master changes, the collector follows the redirect (`HOST_IS_SLAVE` error carries the new master address).

As built (M4): one collector, `xapi.pool`, does both jobs (inventory and performance).

- **Protocol.** JSON-RPC 2.0 at `https://<master>/jsonrpc` (the XAPI wire protocol document), ints read whether sent as numbers or strings. Login with `session.login_with_password` (credential `xapi` in the `auth` role), logout after every run. A pool member answers `HOST_IS_SLAVE` with the master's address, which the collector follows once. Certificates are not verified unless `verify_certificate` is set (hosts ship self-signed ones).
- **Inventory.** `pool`, `host`, `host_metrics`, `VM`, `VM_guest_metrics` and `SR` records, one `get_all_records` call each. Hosts and VMs become child devices (types `hypervisor_host` and `vm`, keys `host:<uuid>` and `vm:<uuid>`); a running VM's parent is its host, a halted one sits under the pool device; a migration moves it. Templates, snapshots and control domains are not devices. The pool device gets the master's product and version as inventory.
- **Performance.** `/rrd_updates?json=true&host=true&cf=AVERAGE&interval=60` from every live host (each serves its own and its VMs' data), the newest value of each source, an older row when the newest is NaN. Host: `cpu_avg`, physical NICs (`pif_eth*`); VM: `cpu*` averaged, `memory` and `memory_internal_free` (needs guest tools), `vif_*`, `vbd_*` read, write and IOPS. `performance: false` skips it.
- **Objects.** `host:<uuid>` (CRITICAL when not live, WARNING when disabled for maintenance), `vm:<uuid>` (power state; paused is WARNING; halted and suspended are `halted_status`, default OK; guest tools as a label), `sr:<uuid>` (size, used percent; SRs without a size such as ISO libraries are skipped). Host and VM objects belong to their child devices, so a VM's incidents and metrics are found on the VM's device. Per-object metrics: `cpu_percent`, `memory_used_percent`, `net_rx_bps`, `net_tx_bps`, `disk_read_bps`, `disk_write_bps`, `disk_iops`, `used_percent`, `size_bytes`, `snapshot_count`, `oldest_snapshot_days`.
- **Tests** run against a fake pool (master and member over TLS) in the documented formats. Not yet verified on our own XCP-ng pool.
- **Suppression.** Host and VM objects are availability objects: a host that is not live (CRITICAL) suppresses its VMs' incidents and the incidents of checks on those VMs; a VM that is down suppresses the checks on it. Maintenance mode (WARNING) suppresses nothing.
- **Not built:** SR multipathing, HA details beyond on/off.

## Facility (SNMP-based)

| Type | Metrics |
| --- | --- |
| `snmp.ups` | UPS-MIB (RFC 1628) plus APC PowerNet profile: `battery_charge_percent`, `runtime_minutes`, `load_percent`, `input_voltage`, `output_voltage`, `on_battery` (status), `battery_replace_needed` (status) |
| `snmp.pdu` | Vendor profiles (APC, Raritan, Eaton to start): per outlet or bank current, total power, per-outlet state |
| `snmp.sensor` | Environmental probes via vendor profiles: temperature, humidity, door, leak |

MQTT and HTTP push sensors are covered by [F08](F08-push-ingestion.md).

As built (M4), APC first, because APC is the facility vendor in our datacenter. OIDs and value meanings were checked against APC's PowerNet-MIB (2025):

- `snmp.pdu` (collector): rPDU2 (AP8xxx and later) with fallback to rPDU (AP78xx, AP79xx). Objects: `pdu:<module>` (power in W, energy in kWh, the PDU's load state), `phase:<n>` (current, voltage, power), `bank:<n>` (current), `outlet:<n>` (on/off for switched outlets, current and power for metered ones). Daisy-chained PDUs: keys of modules other than 1 carry the module (`phase:2.1`), so adding a second PDU keeps the first one's keys. Status is the PDU's own load state: near overload is WARNING, overload CRITICAL. A switched outlet that is off is `outlet_off_status` (default OK). `outlets: false` leaves outlets out. A non-APC device is UNKNOWN for now (Raritan and Eaton profiles when we run them).
- `snmp.sensor` (collector): probes attached to rPDU2 PDUs (temperature and humidity with the PDU's own high/max thresholds; door, smoke, motion, vibration, dry contact and leak sensors) and to network management cards (universal I/O: AP9631 and NMC3 with AP9335T/TH probes, input contacts). Objects `probe:<index>` and `contact:<index>` (`uio.` for NMC sensors). Status follows the device's alarm state; a probe that lost communication is UNKNOWN; sensors not installed or disabled are not objects. Older environmental monitoring units (EMU, `mem*` tables) and NetBotz are not covered.
- Tested against snmpsim fixtures of an AP8853 (rPDU2), an AP7920 (rPDU) and an AP9631 card with a probe and a leak contact. Not yet verified on our own APC units.

## Built-in templates using these plugins

Listed in [F02](F02-devices-credentials-templates.md).

## Acceptance criteria

- Each plugin has `plugintest` coverage against a simulator or recorded fixture: snmpsim for SNMP, the DMTF Redfish mockup server, a MediaMTX RTSP server for cameras, a recorded XAPI session for XCP-ng.
- Each plugin is verified on at least one real device in our datacenter before Gate 1, recorded in a compatibility table in the docs.
- `snmp.interfaces` on a 48-port switch at 60 s interval uses under 1 % of the switch's CPU (measured on our core switch model).

## Open questions

- Which switch, BMC, UPS and PDU vendors do we run in our datacenter? Answered 2026-10-07: Dell, HPE and ASUS servers; APC for PDUs, UPSs and other facility devices. Switch vendors are still open.
