# F02: Devices, credentials, templates and test connection

**Status:** Draft · **Phase:** MVP (templates basic; discovery in phase 2) · **Related:** [ADR-0006](../adr/0006-credential-encryption.md), [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)

## Summary

Devices are the things being monitored, arranged in a tree. Credentials are stored encrypted and never returned. Templates turn "this is a Dell server" into a set of checks. Test connection answers "why doesn't this work?" during onboarding.

## Scope

| MVP | Later |
| --- | --- |
| Device CRUD, bulk create/update, upsert by external ID | Discovery (subnet, ONVIF, LLDP/CDP) |
| Device tree via `parent_id`; collector-created child devices | Automatic topology from LLDP/CDP into the dependency graph |
| Sites; device dependencies set through the API | |
| Credentials: create, update, delete, test; write-only secrets | Credential rotation reminders |
| Templates with checks and collectors, applied to devices | Template versioning and re-apply diff |
| `POST /devices/{id}/test` | |
| Built-in templates for the MVP targets | Community template repository |

## Behavior

### Devices

- Fields: `name`, `address` (IP or hostname), `type` (`network`, `server`, `bmc`, `camera`, `web`, `hypervisor_host`, `vm`, `iot`, `ups`, `pdu`, `sensor`, `other`), `tags` (key/value), `parent_id` (containment only, see [ADR-0014](../adr/0014-device-model-containment-dependencies-sites.md)), `site_id`, `probe_id` (null = core runners), `external_source` / `external_type` / `external_id` (link to the PlusClouds object, such as an IAAS virtual machine or compute member), `inventory` (vendor, model, serial, firmware; written by plugins), `managed_by` (`api` or a collector ID).
- Child devices created by collectors (VMs, interfaces as sub-objects) have `managed_by` set; the API rejects edits to their managed fields but allows tags, notes and external IDs, so PlusClouds can link a discovered VM to its own `VirtualMachines` record.
- VMs are keyed by UUID within their collector, so live migration updates `parent_id` instead of creating a new device.
- A device and a BMC device can be linked (`physical_peer_id`) so a hypervisor host and its iDRAC appear as one physical server.
- Deleting a device deletes its checks, state and child devices; metrics stay until retention removes them. Deletion requires `?confirm=true` when the device has children.

### Credentials

- Types for the MVP: `snmp_v2c`, `snmp_v3`, `redfish`, `ipmi`, `xapi`, `http_basic`, `http_bearer`, `rtsp`, `mqtt`.
- Each type has a JSON Schema (like plugin configs) with secret fields marked `writeOnly`.
- `GET` returns metadata and which fields are set, never values.
- Credentials carry external IDs too, so PlusClouds can upsert the credential it manages for a device without keeping the engine's ID.
- `POST /credentials/{id}/test` with `{ "device_id": … }` tries a minimal authenticated call (SNMP `sysDescr`, Redfish service root with auth, XAPI `session.login_with_password`) and returns success or the exact protocol error.

### Templates

- A template has a name, a `match` hint (device type, vendor), a runbook URL, and a list of check and collector definitions with placeholders (`{{ .Device.Address }}`, `{{ .Credential "bmc" }}`).
- `POST /templates/{id}/apply` with device IDs or a tag selector creates the checks. Checks created from a template record `template_id`; re-applying updates them and reports a diff (dry run by default).
- Built-in templates shipped for the MVP: generic switch (SNMP), Dell iDRAC, HPE iLO, Supermicro BMC, generic IP camera, website, XCP-ng pool, APC UPS, generic PDU.

### Test connection

`POST /devices/{id}/test` runs every enabled check and collector of the device once, synchronously (timeout 60 s), on the runner or probe that owns it, and returns raw results, timings and errors. Results are not stored and do not change state.

## API

`/v1/devices` (CRUD, `bulk`, `by-external-id/{external_id}`, list filters `external_source`, `external_type`, `external_id`, `{id}/children`, `{id}/inventory`, `{id}/test`), `/v1/credentials` (CRUD, `by-external-id/{external_id}`, `{id}/test`), `/v1/templates` (CRUD, `by-external-id/{external_id}`, `{id}/apply`).

## Acceptance criteria

- No API response, log line or audit event contains a credential secret (tested by seeding secrets with a marker string and scanning outputs).
- Bulk create of 1,000 devices completes in under 5 s and is idempotent with the same `Idempotency-Key`.
- Applying the Dell iDRAC template to a Redfish mockup server produces working checks with no manual edits.
- Test connection reports "authentication failed", "timeout" and "connection refused" as distinct errors.

## Decided

- **No credential sharing between tenants.** When the platform tenant (PlusClouds staff) and a customer tenant both monitor the same hardware, each tenant holds its own copy of the credential. Sharing would break the tenant binding of the encryption ([ADR-0006](../adr/0006-credential-encryption.md): `tenant_id` is part of the associated data), give one row two owners under RLS ([ADR-0008](../adr/0008-tenant-isolation.md)), let one tenant's deletion break another tenant's checks, and mix two tenants' audit trails. The cost is rotation in two places; when PlusClouds manages the password, leo4 upserts each copy by external ID ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)).
