-- Sites, devices, dependencies, credentials and checks (F02, ADR-0014).
--
-- References between tenant-owned rows are composite foreign keys on
-- (tenant_id, id): a foreign key check is not subject to row-level security,
-- so a plain key on id alone would let a tenant point at another tenant's row
-- if it knew the UUID.

-- +goose Up

CREATE TABLE sites (
    id              uuid PRIMARY KEY,
    tenant_id       uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name            text        NOT NULL,
    country         text        CHECK (country ~ '^[A-Z]{2}$'),
    timezone        text,
    address         text,
    managed_by      text        NOT NULL DEFAULT 'api',
    external_source text,
    external_type   text,
    external_id     text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name),
    CHECK ((external_source IS NULL) = (external_id IS NULL))
);
CREATE UNIQUE INDEX sites_external ON sites (tenant_id, external_source, coalesce(external_type, ''), external_id)
    WHERE external_id IS NOT NULL;

CREATE TABLE devices (
    id               uuid PRIMARY KEY,
    tenant_id        uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name             text        NOT NULL,
    address          text        NOT NULL DEFAULT '',
    type             text        NOT NULL,
    tags             jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(tags) = 'object'),
    notes            text,
    parent_id        uuid,
    site_id          uuid,
    physical_peer_id uuid,
    inventory        jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(inventory) = 'object'),
    managed_by       text        NOT NULL DEFAULT 'api',
    external_source  text,
    external_type    text,
    external_id      text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name),
    CHECK (parent_id IS DISTINCT FROM id),
    CHECK (physical_peer_id IS DISTINCT FROM id),
    CHECK ((external_source IS NULL) = (external_id IS NULL)),
    FOREIGN KEY (tenant_id, parent_id) REFERENCES devices (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id),
    FOREIGN KEY (tenant_id, physical_peer_id) REFERENCES devices (tenant_id, id) ON DELETE SET NULL (physical_peer_id)
);
CREATE INDEX devices_parent ON devices (tenant_id, parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX devices_site ON devices (tenant_id, site_id) WHERE site_id IS NOT NULL;
CREATE INDEX devices_tags ON devices USING gin (tags);
CREATE UNIQUE INDEX devices_external ON devices (tenant_id, external_source, coalesce(external_type, ''), external_id)
    WHERE external_id IS NOT NULL;

-- Extra "needs to work" edges. Containment (parent_id) is an implied
-- dependency and is not stored here (ADR-0014). Cycles are rejected by the
-- application under a per-tenant lock.
CREATE TABLE device_dependencies (
    tenant_id     uuid        NOT NULL,
    device_id     uuid        NOT NULL,
    depends_on_id uuid        NOT NULL,
    source        text        NOT NULL DEFAULT 'api',
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, device_id, depends_on_id),
    CHECK (device_id <> depends_on_id),
    FOREIGN KEY (tenant_id, device_id) REFERENCES devices (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, depends_on_id) REFERENCES devices (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX device_dependencies_reverse ON device_dependencies (tenant_id, depends_on_id);

-- Secret fields are one encrypted JSON object (ADR-0006); fields holds the
-- non-secret ones and secret_names which secrets are set.
CREATE TABLE credentials (
    id              uuid PRIMARY KEY,
    tenant_id       uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name            text        NOT NULL,
    type            text        NOT NULL,
    fields          jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(fields) = 'object'),
    secret_names    text[]      NOT NULL DEFAULT '{}',
    ciphertext      bytea       NOT NULL,
    nonce           bytea       NOT NULL,
    wrapped_dek     bytea       NOT NULL,
    kek_id          text        NOT NULL,
    managed_by      text        NOT NULL DEFAULT 'api',
    external_source text,
    external_type   text,
    external_id     text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name),
    CHECK ((external_source IS NULL) = (external_id IS NULL))
);
CREATE INDEX credentials_kek ON credentials (kek_id);
CREATE UNIQUE INDEX credentials_external ON credentials (tenant_id, external_source, coalesce(external_type, ''), external_id)
    WHERE external_id IS NOT NULL;

CREATE TABLE checks (
    id                  uuid PRIMARY KEY,
    tenant_id           uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    device_id           uuid        NOT NULL,
    name                text        NOT NULL,
    plugin              text        NOT NULL,
    config              jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config) = 'object'),
    interval_seconds    integer     NOT NULL CHECK (interval_seconds BETWEEN 1 AND 86400),
    timeout_seconds     integer     CHECK (timeout_seconds BETWEEN 1 AND 300),
    enabled             boolean     NOT NULL DEFAULT true,
    thresholds          jsonb       NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(thresholds) = 'array'),
    failure_count       integer     NOT NULL DEFAULT 3 CHECK (failure_count BETWEEN 1 AND 100),
    recovery_count      integer     NOT NULL DEFAULT 1 CHECK (recovery_count BETWEEN 1 AND 100),
    is_host_check       boolean     NOT NULL DEFAULT false,
    unknown_is_critical boolean     NOT NULL DEFAULT false,
    runbook_url         text,
    managed_by          text        NOT NULL DEFAULT 'api',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (device_id, name),
    FOREIGN KEY (tenant_id, device_id) REFERENCES devices (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX checks_tenant ON checks (tenant_id);
CREATE INDEX checks_device ON checks (tenant_id, device_id);
CREATE UNIQUE INDEX checks_one_host_check ON checks (device_id) WHERE is_host_check;

-- Which credential a check uses for each role its plugin knows ("auth", "bmc").
-- A credential in use cannot be deleted.
CREATE TABLE check_credentials (
    tenant_id     uuid NOT NULL,
    check_id      uuid NOT NULL,
    role          text NOT NULL,
    credential_id uuid NOT NULL,
    PRIMARY KEY (tenant_id, check_id, role),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, credential_id) REFERENCES credentials (tenant_id, id)
);
CREATE INDEX check_credentials_credential ON check_credentials (tenant_id, credential_id);

ALTER TABLE sites               ENABLE ROW LEVEL SECURITY;
ALTER TABLE devices             ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_dependencies ENABLE ROW LEVEL SECURITY;
ALTER TABLE credentials         ENABLE ROW LEVEL SECURITY;
ALTER TABLE checks              ENABLE ROW LEVEL SECURITY;
ALTER TABLE check_credentials   ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON sites
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON devices
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON device_dependencies
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON credentials
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON checks
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON check_credentials
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE
    ON sites, devices, device_dependencies, credentials, checks, check_credentials
    TO monitor_app, monitor_system;

-- +goose Down
DROP TABLE check_credentials;
DROP TABLE checks;
DROP TABLE credentials;
DROP TABLE device_dependencies;
DROP TABLE devices;
DROP TABLE sites;
