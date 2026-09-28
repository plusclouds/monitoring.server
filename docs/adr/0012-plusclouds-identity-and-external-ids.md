# ADR-0012: PlusClouds as identity source, and external IDs

**Status:** Proposed · **Date:** 2026-09-27

## Context

The engine works together with the PlusClouds API (`plusclouds.api.v4`, "leo4"). Accounts and users are created and managed in PlusClouds, which also owns roles, billing and personal data. Only minimal user information is passed to the engine. The engine owns everything about monitoring: devices, credentials, checks, webhooks, incidents and metrics.

In PlusClouds, the tenant is an **account** (`iam_accounts`) and a person is a **user** (`iam_users`). Users belong to accounts through `iam_account_users`. Every object exposes a UUID as its public ID. Monitored infrastructure may already exist in PlusClouds as objects such as `NextDeveloper\IAAS\Database\Models\VirtualMachines` or `ComputeMembers`.

The engine must also still run standalone, with no PlusClouds, as the design requires.

## Options considered

### Who owns identity

1. **The engine has its own users and logins.** Duplicates PlusClouds, and personal data would be stored twice. Rejected.
2. **PlusClouds owns identity. The engine keeps a minimal mirror of accounts, users and memberships, keyed by PlusClouds UUIDs.** The engine can still enforce tenancy and attribute actions to users without storing personal data.
3. **No local mirror: trust whatever PlusClouds sends on each request.** Simple, but without tenant rows there is nowhere to keep limits, RLS keys or membership checks, and audit entries would point to nothing.

### How PlusClouds calls the engine

1. **Server to server with a platform key.** leo4 calls the engine with a platform key and names the account and acting user in headers.
2. **Forwarded user token (JWT verified against PlusClouds' keys).** Lets browsers call the engine directly, but needs a token format and key distribution agreed with PlusClouds first.

## Decision

Identity option 2. Calls use option 1 now; option 2 can come in phase 3 if the panel ever calls the engine directly from the browser.

### Mirror of PlusClouds identity

| Engine entity | PlusClouds source | What the engine stores |
| --- | --- | --- |
| Tenant | `iam_accounts` | `id` (engine UUIDv7), `external_source`, `external_id` (account UUID), `name` (display only), `status`, limits |
| User | `iam_users` | `id`, `external_source`, `external_id` (user UUID), `display_name` (optional), `status` |
| Tenant membership | `iam_account_users` + role | `tenant_id`, `user_id`, `role` (`read-only`, `operator`, `admin`) |

The engine does **not** store email, phone number, national ID, birthday or any other personal field. Notifications that must reach a person go out as webhook events and are resolved to people by PlusClouds (or n8n/Node-RED), so the engine never needs contact details.

### Provisioning flow

PlusClouds pushes changes to the engine. Every call is an idempotent upsert keyed by the PlusClouds UUID:

| When this happens in PlusClouds | leo4 calls |
| --- | --- |
| Account enables monitoring | `PUT /v1/tenants/by-external-id/{account_uuid}` with name and plan limits |
| User joins the account or changes role | `PUT /v1/tenants/{tenant}/members/by-external-id/{user_uuid}` with role and optional display name |
| User leaves the account | `DELETE /v1/tenants/{tenant}/members/by-external-id/{user_uuid}` |
| User is deactivated globally | `PATCH /v1/users/by-external-id/{user_uuid}` with `status: disabled` |
| Account is suspended | `PATCH /v1/tenants/by-external-id/{account_uuid}` with `status: suspended` |
| Account is deleted | `DELETE /v1/tenants/by-external-id/{account_uuid}` |

- **Just-in-time provisioning.** If a platform-key request names an account or user the engine has not seen, the engine creates it with default limits and `read-only` role, and flags it `provisioned: jit`. This covers missed sync calls. Roles above `read-only` always need an explicit membership call.
- **Suspended tenants:** checks stop, API is read-only, data is kept.
- **Deleted tenants:** soft-deleted immediately (checks stop, API returns 404), purged after a grace period set at platform level. Metrics go away through normal retention.
- **Reconciliation:** `GET /v1/tenants?external_source=plusclouds` and `GET /v1/tenants/{id}/members` let leo4 run a periodic comparison job and fix drift.

### Requests on behalf of a user

leo4 calls the engine with:

```http
Authorization: Bearer mon_plat_…            (platform key)
X-Tenant-External-ID: <iam_accounts.uuid>   (or X-Tenant-ID with the engine's own ID)
X-Actor-External-ID: <iam_users.uuid>       (the user who clicked the button)
```

- The engine resolves the tenant and user, checks the membership role, and applies RLS as with any other key.
- Requests without `X-Actor-External-ID` run as the platform itself (for background jobs in leo4), with the platform key's own role.
- Audit events record both: `actor = user <engine id> (plusclouds:<user uuid>) via platform key <key id>`.
- **The leo4 client** follows the pattern PlusClouds already uses for service-to-service calls to its AI service (`AIAssistanceService` with an `envelope` of `{ account, user }`): a `MonitoringService::request($path, $data, $method, $envelope)` class maps `$envelope['account']` to `X-Tenant-External-ID` and `$envelope['user']` to `X-Actor-External-ID`. The engine takes these as headers, not as a body field, because `GET` and `DELETE` have no body and request schemas stay free of auth data.
- Differences from the AI service's `InternalAppAuthenticate`, on purpose: the engine accepts **UUIDs only**, never PlusClouds' internal integer IDs; an actor that is not a member of the tenant gets `403` instead of the request silently running as the account; the platform key is IP-restricted and every use is audited with both identities.
- **Standalone installs** do not use any of this. They use tenant API keys as in [ADR-0007](0007-api-spec-first-openapi.md), and tenants, users and memberships are created locally with no `external_source`.

### External IDs on every resource

Every resource that another system might create or link to has the same three optional fields:

| Field | Meaning | Example |
| --- | --- | --- |
| `external_source` | Which system the ID belongs to | `plusclouds` |
| `external_type` | Object type in that system, following the PlusClouds convention of the full model class | `NextDeveloper\IAAS\Database\Models\VirtualMachines` |
| `external_id` | The object's ID in that system (text, so non-UUID systems work too) | `9b1c…` |

Rules:

- Unique per tenant on `(external_source, external_type, external_id)` when set. A partial unique index ignores rows where it is null.
- Every such resource has `PUT /v1/<resource>/by-external-id/{external_id}?source=&type=` for idempotent upserts, and list endpoints accept `external_source`, `external_type` and `external_id` filters.
- The fields are set by the client and never interpreted by the engine, except for tenants and users, where they drive the identity mirror above.
- The engine's own UUIDv7 stays the primary ID everywhere. External IDs are a lookup key, never a foreign key inside the engine.

Resources that carry external IDs:

| Resource | Typical PlusClouds link |
| --- | --- |
| Tenant | `iam_accounts` |
| User | `iam_users` |
| Device | IAAS virtual machine, compute member, network device or a PlusClouds-side monitoring record |
| Credential | a PlusClouds secret or vault reference, if PlusClouds manages the device password |
| Webhook endpoint | a PlusClouds notification channel or integration record |
| Alert route | a PlusClouds notification rule |
| Probe | a PlusClouds site or datacenter (`Datacenters`) |
| Maintenance window | a PlusClouds maintenance or change record |
| Template | a PlusClouds product or plan that implies a template |

Checks, incidents and collector-created child devices do not need client-set external IDs: checks belong to a device, incidents are created by the engine, and VMs discovered by collectors are keyed by hypervisor UUID. PlusClouds can still link a discovered VM to its own record by setting the external ID on that child device afterwards (a tag-level edit that the API allows on managed devices).

### External IDs in outgoing events

Webhook and SSE events include the external identifiers of the tenant, the device (and its parent path), and the acting user (for acknowledgements), next to the engine's own IDs. PlusClouds can then act on an event without looking anything up. The envelope is the CloudEvents 1.0 shape PlusClouds already uses for its own events, with `data.account_id` set to the PlusClouds account UUID ([ADR-0009](0009-webhook-delivery.md)).

### PlusClouds receiver

leo4 receives engine events on one endpoint, verifies the Standard Webhooks signature with the code its `event_webhook` pusher already uses, and **re-fires each event through its own event system** (`Events::fire`). From there, PlusClouds' existing listeners and pushers deliver it: email and SMS (`event_message`), the panel inbox (`event_inapp`), chat (`event_chat`), CRM (`event_crm_opportunity`) and the NATS live stream to the panel (`client.{account_uuid}.evt`).

- The engine keeps one outgoing channel. Who is notified, through what and when is configured with PlusClouds listeners (`conditions`, `time_window`, recipient accounts), not in the engine.
- The panel gets live monitoring updates over its existing NATS stream, without connecting to the engine's SSE stream.
- `Events::fire` expects an Eloquent model today, so leo4 needs either a small mirror model for monitoring incidents or a variant that fires from an envelope. This is PlusClouds-side work.
- leo4 registers its receiver as a webhook endpoint of each tenant through `PUT /v1/webhooks/by-external-id/…` when the account enables monitoring.

## Consequences

- No personal data in the engine beyond an optional display name, which keeps its security and privacy scope small.
- PlusClouds must call the engine whenever accounts, memberships or roles change. JIT provisioning and the reconciliation endpoints limit the damage of a missed call, but a missed role *downgrade* stays in effect until reconciliation runs. The reconciliation job should run at least hourly.
- A leaked platform key can act as any user in any tenant. It must be stored only in leo4's server-side secrets, restricted by IP allowlist, and rotated regularly. Every use is audited.
- Standalone and PlusClouds-connected installs share one code path; the only difference is whether tenants and users carry `external_source`.
