# F01: Tenancy, identity, API keys, roles and audit log

**Status:** Draft · **Phase:** MVP · **Related:** [ADR-0007](../adr/0007-api-spec-first-openapi.md), [ADR-0008](../adr/0008-tenant-isolation.md), [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)

## Summary

Every object in the engine belongs to a tenant. When connected to PlusClouds, a tenant is a PlusClouds account and users are PlusClouds users. The engine keeps only a minimal mirror of them, keyed by PlusClouds UUIDs. Access is through a platform key (PlusClouds acting for a user) or through tenant API keys (scripts and standalone installs). Every write is recorded in an immutable audit log.

## Scope

| MVP | Later |
| --- | --- |
| Tenants with limits and external IDs | Forwarded user tokens (JWT) if the panel calls the engine from the browser |
| Minimal user mirror and tenant memberships with roles | Custom roles |
| Provisioning API for PlusClouds (idempotent upserts, JIT fallback, reconciliation) | |
| Platform key acting for a user via headers | |
| Tenant API keys with roles `read-only`, `operator`, `admin` | |
| RLS on every tenant table | Per-tenant Grafana logins (phase 3) |
| Audit log for every write, queryable via API | Audit export to external SIEM |
| Bootstrap command for the platform key and first tenant | |

## Data

| Entity | Fields |
| --- | --- |
| `tenants` | `id`, `name`, `status` (`active`, `suspended`, `deleted`), limits, `external_source`, `external_type`, `external_id`, `provisioned` (`api`, `jit`), timestamps |
| `users` | `id`, `display_name` (optional), `status` (`active`, `disabled`), `external_source`, `external_type`, `external_id`, `provisioned`, timestamps. **No email, phone or other personal data.** |
| `tenant_members` | `tenant_id`, `user_id`, `role`, timestamps |
| `api_keys` | `id`, `tenant_id` (null for platform keys), `kind` (`tenant`, `platform`), `name`, `prefix`, `hash`, `role`, `expires_at`, `ip_allowlist`, `last_used_at` |
| `audit_events` | see below |

Users are global (one row per PlusClouds user) and join tenants through `tenant_members`, matching `iam_account_users` in PlusClouds.

## Behavior

### Roles

| Role | Can |
| --- | --- |
| `read-only` | Read everything in the tenant except secrets; read metrics and incidents; subscribe to events |
| `operator` | Everything above, plus acknowledge/resolve/comment incidents, create maintenance windows, run checks now, test devices |
| `admin` | Everything above, plus manage devices, checks, credentials, templates, routes, webhooks, retention, tenant API keys |
| platform | Provision tenants, users and memberships; act on any tenant as a named user |

### Resolving the caller

1. Tenant API key: tenant and role come from the key.
2. Platform key with `X-Tenant-External-ID` (or `X-Tenant-ID`) and `X-Actor-External-ID`: the engine resolves the tenant and user (creating them just in time if unknown, see [ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)), then uses the user's membership role in that tenant. A user with no membership gets `403`.
3. Platform key with a tenant but no actor: acts as the platform with full rights in that tenant (for leo4 background jobs).
4. Suspended tenant: every write returns `403` with a `tenant-suspended` problem type; reads still work.

### Tenant limits

Stored on the tenant, set by PlusClouds from the account's plan when provisioning, enforced on write, returned by `GET /v1/tenant`: `max_devices`, `max_checks`, `min_check_interval_seconds`, `api_rate_per_minute`, `allowed_target_networks` (SSRF guard for HTTP checks and webhooks; default denies RFC 1918, loopback and link-local for customer tenants), `metric_classes` (which retention classes the tenant may use).

### Audit log

- Written in the same transaction as the change it describes.
- Fields: `id`, `tenant_id`, `actor_user_id` and its `external_id` (when a user acted), `api_key_id`, `action` (`device.update`), `object_type`, `object_id`, `before`, `after` (JSON, secrets replaced by `"[changed]"`), `request_id`, `source_ip`, `at`.
- The application database role has `INSERT` and `SELECT` only on the audit table; no `UPDATE` or `DELETE`. Retention is set by a platform-level policy and applied by partition drop.

### Bootstrap

`monitor admin bootstrap` creates the platform key for leo4 and prints it once. `monitor admin bootstrap --standalone --tenant "Name"` instead creates a local tenant and an admin API key, for installs without PlusClouds. Both refuse to run twice.

## API

| Endpoint | Caller |
| --- | --- |
| `PUT /v1/tenants/by-external-id/{external_id}`, `PATCH`, `DELETE` on the same path | platform |
| `GET /v1/tenants?external_source=&external_id=` | platform |
| `PUT/DELETE /v1/tenants/{id}/members/by-external-id/{user_external_id}`, `GET /v1/tenants/{id}/members` | platform |
| `PATCH /v1/users/by-external-id/{external_id}` | platform |
| `GET /v1/tenant`, `GET /v1/me` (resolved tenant, user and role) | any |
| `POST/GET /v1/api-keys`, `DELETE /v1/api-keys/{id}` | tenant admin |
| `GET /v1/audit?object_type=&object_id=&actor=&from=&to=` | tenant admin |

## Acceptance criteria

- Calling the tenant and membership upserts twice with the same body changes nothing and writes no second audit row.
- A request for an unknown PlusClouds account with a platform key creates exactly one tenant, even under 20 concurrent requests.
- A user removed from an account in PlusClouds (membership `DELETE`) gets `403` on the next request.
- A key from tenant A gets `404` (not `403`) for any object in tenant B.
- With application filtering deliberately removed from one repository function in a test build, cross-tenant reads still return nothing (RLS works).
- Every write endpoint in the OpenAPI spec produces exactly one audit row; a CI test walks the spec to check this.
- No personal data field (email, phone) exists in the schema; a migration test fails if one is added to `users`.

## Open questions

- Should PlusClouds roles map to engine roles one-to-one, or should leo4 compute the engine role from PlusClouds permissions? Proposed: leo4 computes and sends one of the three engine roles, so the engine does not need to know PlusClouds' role model.
- Grace period before a deleted tenant is purged.
