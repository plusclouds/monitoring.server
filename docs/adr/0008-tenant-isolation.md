# ADR-0008: Tenant isolation

**Status:** Proposed · **Date:** 2026-09-27

## Context

Customers use the engine, so tenant isolation is part of the core. The design requires isolation to be enforced in every query, including metric labels. Grafana reads PostgreSQL directly, which bypasses the API.

## Options considered

1. **Database per tenant.** Strongest isolation, but thousands of schemas or databases, migrations per tenant, and no shared scheduler. Too heavy.
2. **Shared tables, application-level filtering only.** Simple, but a single missing `WHERE tenant_id = …` leaks data.
3. **Shared tables, application filtering plus PostgreSQL row-level security (RLS).** The application still filters, and the database refuses rows from other tenants if it ever forgets.

## Decision

Option 3.

- Every tenant-owned table has `tenant_id uuid NOT NULL` and an RLS policy `USING (tenant_id = current_setting('app.tenant_id')::uuid)`.
- The application connects as a role that is subject to RLS (not the table owner, not `BYPASSRLS`). Every request runs in a transaction that starts with `SET LOCAL app.tenant_id = …`. A repository helper makes this the only way to get a database handle in request code.
- Background roles that must see all tenants (runner, engine, maintenance, notifier) use a separate database role with `BYPASSRLS`, and only in those components. A test asserts that the API code path never gets that role.
- **Metrics:** `metric_groups` and `metric_series` carry `tenant_id` and have RLS. Sample and rollup tables do not carry a tenant column (it would add ~8 % to storage); they are reachable only through the RLS-protected series and group tables, via the views from [ADR-0003](0003-metrics-storage-layout.md) created with `security_barrier`.
- **Grafana:**
  - Internal (MVP): one Grafana instance for PlusClouds staff using a read-only role with access to all tenants.
  - Customers (phase 3): one read-only PostgreSQL login per tenant, with `app.tenant_id` fixed for that role (`ALTER ROLE … SET app.tenant_id = …`) and grants only on the views. The engine's API creates and rotates these logins.
- **Tenant limits** (devices, checks, minimum interval, API rate) live on the tenant row and are enforced by the API on write.

## Consequences

- A bug in application filtering results in empty responses, not leaked data.
- Every new table needs a policy; a migration test fails if a table with `tenant_id` has RLS disabled.
- RLS adds a small planning cost to every query; measured in the load test.
- Customer Grafana access via per-tenant database logins must be validated for query-cost abuse (a tenant can run heavy SQL); per-role `statement_timeout` and connection limits mitigate this.
