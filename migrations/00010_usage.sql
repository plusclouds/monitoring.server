-- Usage metering (F13, milestone M3.5).
--
-- Billable time is recorded as periods, kept by triggers on checks, devices
-- and tenants. Every way a check can be created, enabled, disabled, moved
-- to another interval or deleted, and every tenant suspend, resume, delete
-- or restore, ends in a write to those tables, so the periods follow in the
-- same transaction without each code path having to remember them.
--
-- The maintenance role closes each UTC hour into usage_hours (one row per
-- tenant) and usage_hour_plugins (its breakdown). Closed hours are never
-- rewritten; a correction is a new revision.

-- +goose Up

-- A check is billable while it is enabled and its tenant is active. The
-- platform tenant (self-monitoring) is never billed.
CREATE TABLE check_periods (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id        uuid        NOT NULL REFERENCES tenants (id),
    check_id         uuid        NOT NULL,   -- no foreign key: periods outlive deleted checks
    device_id        uuid        NOT NULL,
    plugin           text        NOT NULL,
    interval_seconds integer     NOT NULL,
    started_at       timestamptz NOT NULL,
    ended_at         timestamptz,
    CHECK (ended_at IS NULL OR ended_at >= started_at)
);
CREATE UNIQUE INDEX check_periods_open ON check_periods (check_id) WHERE ended_at IS NULL;
CREATE INDEX check_periods_time ON check_periods (started_at, ended_at);

-- A device counts (informationally) while it exists and its tenant is active.
CREATE TABLE device_periods (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id  uuid        NOT NULL REFERENCES tenants (id),
    device_id  uuid        NOT NULL,
    started_at timestamptz NOT NULL,
    ended_at   timestamptz,
    CHECK (ended_at IS NULL OR ended_at >= started_at)
);
CREATE UNIQUE INDEX device_periods_open ON device_periods (device_id) WHERE ended_at IS NULL;
CREATE INDEX device_periods_time ON device_periods (started_at, ended_at);

-- Weight history. plugin '*' is the default weight; a NULL weight means the
-- plugin falls back to the default from effective_from on.
CREATE TABLE usage_weights (
    plugin         text          NOT NULL,
    effective_from timestamptz   NOT NULL,
    weight         numeric(12,3) CHECK (weight >= 0),
    recorded_at    timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (plugin, effective_from),
    CHECK (plugin <> '*' OR weight IS NOT NULL)
);

CREATE TABLE usage_hours (
    tenant_id              uuid          NOT NULL REFERENCES tenants (id),
    period_start           timestamptz   NOT NULL,
    revision               integer       NOT NULL CHECK (revision >= 1),
    account_id             text,         -- the tenant's external ID when the hour was closed
    billable_check_seconds numeric(20,3) NOT NULL,
    device_seconds         bigint        NOT NULL,
    reason                 text,         -- why a revision above 1 was written
    closed_at              timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, period_start, revision)
);
CREATE INDEX usage_hours_period ON usage_hours (period_start);

CREATE TABLE usage_hour_plugins (
    tenant_id     uuid          NOT NULL,
    period_start  timestamptz   NOT NULL,
    revision      integer       NOT NULL,
    plugin        text          NOT NULL,
    count_seconds bigint        NOT NULL,
    weight        numeric(12,3) NOT NULL,
    PRIMARY KEY (tenant_id, period_start, revision, plugin),
    FOREIGN KEY (tenant_id, period_start, revision) REFERENCES usage_hours ON DELETE CASCADE
);

-- Hours before closed_until are closed.
CREATE TABLE usage_close (
    singleton    boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    closed_until timestamptz NOT NULL
);

-- +goose StatementBegin
-- Brings the open period of one check in line with the check's state.
CREATE FUNCTION usage_sync_check(p_check uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    c_tenant   uuid;
    c_device   uuid;
    c_plugin   text;
    c_interval integer;
    c_billable boolean := false;
    o_id       bigint;
    o_device   uuid;
    o_plugin   text;
    o_interval integer;
BEGIN
    SELECT ch.tenant_id, ch.device_id, ch.plugin, ch.interval_seconds,
           ch.enabled AND t.status = 'active' AND NOT t.is_platform
      INTO c_tenant, c_device, c_plugin, c_interval, c_billable
      FROM public.checks ch JOIN public.tenants t ON t.id = ch.tenant_id
     WHERE ch.id = p_check;
    c_billable := coalesce(c_billable, false);
    SELECT id, device_id, plugin, interval_seconds INTO o_id, o_device, o_plugin, o_interval
      FROM public.check_periods WHERE check_id = p_check AND ended_at IS NULL FOR UPDATE;
    IF o_id IS NOT NULL AND (NOT c_billable OR o_plugin <> c_plugin
            OR o_interval <> c_interval OR o_device <> c_device) THEN
        UPDATE public.check_periods SET ended_at = greatest(now(), started_at) WHERE id = o_id;
        o_id := NULL;
    END IF;
    IF o_id IS NULL AND c_billable THEN
        INSERT INTO public.check_periods (tenant_id, check_id, device_id, plugin, interval_seconds, started_at)
        VALUES (c_tenant, p_check, c_device, c_plugin, c_interval, now());
    END IF;
END
$$;

CREATE FUNCTION usage_sync_device(p_device uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    d_tenant  uuid;
    d_counted boolean := false;
    o_id      bigint;
BEGIN
    SELECT dv.tenant_id, t.status = 'active' AND NOT t.is_platform INTO d_tenant, d_counted
      FROM public.devices dv JOIN public.tenants t ON t.id = dv.tenant_id
     WHERE dv.id = p_device;
    d_counted := coalesce(d_counted, false);
    SELECT id INTO o_id FROM public.device_periods WHERE device_id = p_device AND ended_at IS NULL FOR UPDATE;
    IF o_id IS NOT NULL AND NOT d_counted THEN
        UPDATE public.device_periods SET ended_at = greatest(now(), started_at) WHERE id = o_id;
    ELSIF o_id IS NULL AND d_counted THEN
        INSERT INTO public.device_periods (tenant_id, device_id, started_at) VALUES (d_tenant, p_device, now());
    END IF;
END
$$;

CREATE FUNCTION usage_on_check() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
BEGIN
    PERFORM public.usage_sync_check(coalesce(NEW.id, OLD.id));
    RETURN NULL;
END
$$;

CREATE FUNCTION usage_on_device() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
BEGIN
    PERFORM public.usage_sync_device(coalesce(NEW.id, OLD.id));
    RETURN NULL;
END
$$;

CREATE FUNCTION usage_on_tenant() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    id uuid;
BEGIN
    FOR id IN SELECT c.id FROM public.checks c WHERE c.tenant_id = NEW.id LOOP
        PERFORM public.usage_sync_check(id);
    END LOOP;
    FOR id IN SELECT d.id FROM public.devices d WHERE d.tenant_id = NEW.id LOOP
        PERFORM public.usage_sync_device(id);
    END LOOP;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER usage_periods AFTER INSERT OR DELETE OR UPDATE OF enabled, plugin, interval_seconds, device_id ON checks
    FOR EACH ROW EXECUTE FUNCTION usage_on_check();
CREATE TRIGGER usage_periods AFTER INSERT OR DELETE ON devices
    FOR EACH ROW EXECUTE FUNCTION usage_on_device();
CREATE TRIGGER usage_periods AFTER UPDATE OF status ON tenants
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION usage_on_tenant();

-- Counting starts with this migration: what exists now opens a period now.
SELECT usage_sync_check(id) FROM checks;
SELECT usage_sync_device(id) FROM devices;
INSERT INTO usage_close (closed_until) VALUES (date_trunc('hour', now(), 'UTC'));

-- +goose StatementBegin
-- Closes one UTC hour as revision p_revision and returns the number of
-- tenant rows written. Revision 1 does nothing for an hour that is already
-- closed; a higher revision is a correction of every tenant in the hour.
CREATE FUNCTION usage_close_hour(p_start timestamptz, p_revision integer, p_reason text) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    p_end timestamptz := p_start + interval '1 hour';
    n     integer;
BEGIN
    IF p_start <> date_trunc('hour', p_start, 'UTC') THEN
        RAISE EXCEPTION 'hour % is not aligned', p_start;
    END IF;
    IF p_end > now() THEN
        RAISE EXCEPTION 'hour % has not ended', p_start;
    END IF;
    IF p_revision = 1 AND EXISTS (SELECT 1 FROM public.usage_hours WHERE period_start = p_start) THEN
        RETURN 0;
    END IF;

    CREATE TEMP TABLE usage_tmp ON COMMIT DROP AS
    WITH w AS (
        SELECT DISTINCT ON (plugin) plugin, weight
          FROM public.usage_weights WHERE effective_from <= p_start
         ORDER BY plugin, effective_from DESC
    ),
    secs AS (
        SELECT tenant_id, plugin,
               sum(extract(epoch FROM least(coalesce(ended_at, p_end), p_end) - greatest(started_at, p_start)))::bigint AS s
          FROM public.check_periods
         WHERE started_at < p_end AND coalesce(ended_at, 'infinity') > p_start
         GROUP BY 1, 2
    )
    SELECT s.tenant_id, s.plugin, s.s AS count_seconds,
           coalesce((SELECT weight FROM w WHERE w.plugin = s.plugin),
                    (SELECT weight FROM w WHERE w.plugin = '*'), 1) AS weight
      FROM secs s;

    INSERT INTO public.usage_hours (tenant_id, period_start, revision, account_id, billable_check_seconds,
                                    device_seconds, reason)
    SELECT t.id, p_start, p_revision, t.external_id,
           coalesce((SELECT sum(u.count_seconds * u.weight) FROM usage_tmp u WHERE u.tenant_id = t.id), 0),
           coalesce((SELECT sum(extract(epoch FROM least(coalesce(d.ended_at, p_end), p_end) - greatest(d.started_at, p_start)))::bigint
                       FROM public.device_periods d
                      WHERE d.tenant_id = t.id AND d.started_at < p_end AND coalesce(d.ended_at, 'infinity') > p_start), 0),
           p_reason
      FROM public.tenants t
     WHERE EXISTS (SELECT 1 FROM usage_tmp u WHERE u.tenant_id = t.id)
        OR EXISTS (SELECT 1 FROM public.device_periods d
                    WHERE d.tenant_id = t.id AND d.started_at < p_end AND coalesce(d.ended_at, 'infinity') > p_start);
    GET DIAGNOSTICS n = ROW_COUNT;

    INSERT INTO public.usage_hour_plugins (tenant_id, period_start, revision, plugin, count_seconds, weight)
    SELECT tenant_id, p_start, p_revision, plugin, count_seconds, weight FROM usage_tmp;
    DROP TABLE usage_tmp;
    RETURN n;
END
$$;

-- Latest revision of every closed hour in [p_from, p_to) with billable
-- usage, for the platform key's billing pull. Keyset-paginated by
-- (period_start, account_id).
CREATE FUNCTION usage_tenant_hours(p_from timestamptz, p_to timestamptz, p_after_period timestamptz,
                                   p_after_account text, p_limit integer)
RETURNS TABLE (account_id text, period_start timestamptz, revision integer, billable_check_seconds numeric,
               device_seconds bigint, breakdown jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
    SELECT h.account_id, h.period_start, h.revision, h.billable_check_seconds, h.device_seconds,
           coalesce((SELECT jsonb_object_agg(p.plugin, jsonb_build_object('count_seconds', p.count_seconds, 'weight', p.weight))
                       FROM public.usage_hour_plugins p
                      WHERE p.tenant_id = h.tenant_id AND p.period_start = h.period_start AND p.revision = h.revision),
                    '{}'::jsonb)
      FROM (SELECT DISTINCT ON (tenant_id, period_start) *
              FROM public.usage_hours
             WHERE period_start >= p_from AND period_start < p_to
             ORDER BY tenant_id, period_start, revision DESC) h
     WHERE h.account_id IS NOT NULL AND h.billable_check_seconds > 0
       AND (p_after_period IS NULL OR (h.period_start, h.account_id) > (p_after_period, p_after_account))
     ORDER BY h.period_start, h.account_id
     LIMIT p_limit
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION usage_sync_check(uuid), usage_sync_device(uuid), usage_close_hour(timestamptz, integer, text),
    usage_tenant_hours(timestamptz, timestamptz, timestamptz, text, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION usage_close_hour(timestamptz, integer, text) TO monitor_system;
GRANT EXECUTE ON FUNCTION usage_tenant_hours(timestamptz, timestamptz, timestamptz, text, integer) TO monitor_app, monitor_system;

ALTER TABLE usage_hours ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_hour_plugins ENABLE ROW LEVEL SECURITY;
ALTER TABLE check_periods ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_periods ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON usage_hours
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON usage_hour_plugins
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON check_periods
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON device_periods
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT ON usage_hours, usage_hour_plugins, check_periods, usage_weights, usage_close TO monitor_app;
GRANT SELECT ON check_periods, device_periods, usage_hours, usage_hour_plugins, usage_close TO monitor_system;
GRANT SELECT, INSERT, UPDATE ON usage_weights TO monitor_system;
GRANT INSERT, UPDATE ON usage_close TO monitor_system;
GRANT DELETE ON usage_hours, usage_hour_plugins TO monitor_system;   -- usage retention

-- +goose Down
DROP TRIGGER usage_periods ON tenants;
DROP TRIGGER usage_periods ON devices;
DROP TRIGGER usage_periods ON checks;
DROP FUNCTION usage_tenant_hours(timestamptz, timestamptz, timestamptz, text, integer);
DROP FUNCTION usage_close_hour(timestamptz, integer, text);
DROP FUNCTION usage_on_tenant();
DROP FUNCTION usage_on_device();
DROP FUNCTION usage_on_check();
DROP FUNCTION usage_sync_device(uuid);
DROP FUNCTION usage_sync_check(uuid);
DROP TABLE usage_close, usage_hour_plugins, usage_hours, usage_weights, device_periods, check_periods;
