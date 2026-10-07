-- v0.8.0: billable collector objects (each XCP-ng pool host bills as
-- "xapi.pool:host") and the Whoopsy! band the engine used for each result.

-- +goose Up

-- The billing key of a check object ("xapi.pool:host"); null when the
-- object is not billed on its own (collector objects are not, F13 rule 11,
-- except the kinds a plugin declares).
ALTER TABLE check_objects ADD COLUMN billing_key text;

-- Billable time of collector objects, like check_periods for checks.
CREATE TABLE object_periods (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   uuid        NOT NULL REFERENCES tenants (id),
    check_id    uuid        NOT NULL,   -- no foreign key: periods outlive deleted checks
    object_key  text        NOT NULL,
    billing_key text        NOT NULL,
    started_at  timestamptz NOT NULL,
    ended_at    timestamptz,
    CHECK (ended_at IS NULL OR ended_at >= started_at)
);
CREATE UNIQUE INDEX object_periods_open ON object_periods (check_id, object_key) WHERE ended_at IS NULL;
CREATE INDEX object_periods_time ON object_periods (started_at, ended_at);
ALTER TABLE object_periods ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON object_periods
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT ON object_periods TO monitor_app, monitor_system;

-- +goose StatementBegin
-- Opens and ends the periods of a check's billable objects: open while the
-- check is billable and the object is reported, ended otherwise.
CREATE FUNCTION usage_sync_objects(p_check uuid, p_billable boolean) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
BEGIN
    UPDATE public.object_periods p SET ended_at = greatest(now(), p.started_at)
     WHERE p.check_id = p_check AND p.ended_at IS NULL
       AND (NOT p_billable OR NOT EXISTS (
            SELECT 1 FROM public.check_objects o
             WHERE o.check_id = p_check AND o.object_key = p.object_key AND o.gone_at IS NULL
               AND o.billing_key = p.billing_key));
    IF p_billable THEN
        INSERT INTO public.object_periods (tenant_id, check_id, object_key, billing_key, started_at)
        SELECT o.tenant_id, o.check_id, o.object_key, o.billing_key, now()
          FROM public.check_objects o
         WHERE o.check_id = p_check AND o.billing_key IS NOT NULL AND o.gone_at IS NULL
           AND NOT EXISTS (SELECT 1 FROM public.object_periods p
                            WHERE p.check_id = p_check AND p.object_key = o.object_key AND p.ended_at IS NULL);
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- For the engine after a collector run: billable as usage_sync_check
-- decides (enabled check, active tenant, not the platform).
CREATE FUNCTION usage_sync_check_objects(p_check uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    b boolean;
BEGIN
    SELECT ch.enabled AND t.status = 'active' AND NOT t.is_platform INTO b
      FROM public.checks ch JOIN public.tenants t ON t.id = ch.tenant_id WHERE ch.id = p_check;
    PERFORM public.usage_sync_objects(p_check, coalesce(b, false));
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION usage_sync_objects(uuid, boolean), usage_sync_check_objects(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION usage_sync_check_objects(uuid) TO monitor_system;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION usage_sync_check(p_check uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    c_tenant   uuid;
    c_device   uuid;
    c_plugin   text;
    c_interval integer;
    c_whoopsy  boolean;
    c_billable boolean := false;
    o_id       bigint;
    o_device   uuid;
    o_plugin   text;
    o_interval integer;
    o_whoopsy  boolean;
BEGIN
    SELECT ch.tenant_id, ch.device_id, ch.plugin, ch.interval_seconds, ch.whoopsy IS NOT NULL,
           ch.enabled AND t.status = 'active' AND NOT t.is_platform
      INTO c_tenant, c_device, c_plugin, c_interval, c_whoopsy, c_billable
      FROM public.checks ch JOIN public.tenants t ON t.id = ch.tenant_id
     WHERE ch.id = p_check;
    c_billable := coalesce(c_billable, false);
    SELECT id, device_id, plugin, interval_seconds, whoopsy INTO o_id, o_device, o_plugin, o_interval, o_whoopsy
      FROM public.check_periods WHERE check_id = p_check AND ended_at IS NULL FOR UPDATE;
    IF o_id IS NOT NULL AND (NOT c_billable OR o_plugin <> c_plugin OR o_interval <> c_interval
            OR o_device <> c_device OR o_whoopsy <> c_whoopsy) THEN
        UPDATE public.check_periods SET ended_at = greatest(now(), started_at) WHERE id = o_id;
        o_id := NULL;
    END IF;
    IF o_id IS NULL AND c_billable THEN
        INSERT INTO public.check_periods (tenant_id, check_id, device_id, plugin, interval_seconds, whoopsy, started_at)
        VALUES (c_tenant, p_check, c_device, c_plugin, c_interval, c_whoopsy, now());
    END IF;
    PERFORM public.usage_sync_objects(p_check, c_billable);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION usage_close_hour(p_start timestamptz, p_revision integer, p_reason text) RETURNS integer
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
        SELECT tenant_id, plugin, whoopsy,
               sum(extract(epoch FROM least(coalesce(ended_at, p_end), p_end) - greatest(started_at, p_start)))::bigint AS s
          FROM public.check_periods
         WHERE started_at < p_end AND coalesce(ended_at, 'infinity') > p_start
         GROUP BY 1, 2, 3
    )
    -- A check with Whoopsy! is billed as "<plugin>+whoopsy": its own weight
    -- when the config sets one, else the plugin's weight times the
    -- multiplier "*whoopsy" (default 5).
    SELECT s.tenant_id, s.plugin || CASE WHEN s.whoopsy THEN '+whoopsy' ELSE '' END AS plugin, s.s AS count_seconds,
           CASE WHEN s.whoopsy THEN
               coalesce((SELECT weight FROM w WHERE w.plugin = s.plugin || '+whoopsy'),
                        coalesce((SELECT weight FROM w WHERE w.plugin = s.plugin),
                                 (SELECT weight FROM w WHERE w.plugin = '*'), 1)
                        * coalesce((SELECT weight FROM w WHERE w.plugin = '*whoopsy'), 5))
           ELSE
               coalesce((SELECT weight FROM w WHERE w.plugin = s.plugin),
                        (SELECT weight FROM w WHERE w.plugin = '*'), 1)
           END AS weight
      FROM secs s;

    -- Billable collector objects (xapi.pool hosts), under "<plugin>:<kind>".
    INSERT INTO usage_tmp (tenant_id, plugin, count_seconds, weight)
    WITH w AS (
        SELECT DISTINCT ON (plugin) plugin, weight
          FROM public.usage_weights WHERE effective_from <= p_start
         ORDER BY plugin, effective_from DESC
    )
    SELECT o.tenant_id, o.billing_key,
           sum(extract(epoch FROM least(coalesce(o.ended_at, p_end), p_end) - greatest(o.started_at, p_start)))::bigint,
           coalesce((SELECT weight FROM w WHERE w.plugin = o.billing_key),
                    (SELECT weight FROM w WHERE w.plugin = '*'), 1)
      FROM public.object_periods o
     WHERE o.started_at < p_end AND coalesce(o.ended_at, 'infinity') > p_start
     GROUP BY 1, 2;

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
-- +goose StatementEnd

-- The Whoopsy! band the engine judged each result against (premium
-- alerting), for drawing it: one row per result while Whoopsy! is on.
CREATE TABLE whoopsy_band (
    tenant_id   uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    check_id    uuid        NOT NULL,
    ts          timestamptz NOT NULL,
    value       float8      NOT NULL,
    mean        float8,     -- null while the band is learning
    stddev      float8,
    lower_bound float8,     -- null when the direction does not watch that side
    upper_bound float8,
    outside     boolean     NOT NULL,
    hits        integer     NOT NULL,
    alerting    boolean     NOT NULL,
    PRIMARY KEY (check_id, ts),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX whoopsy_band_ts ON whoopsy_band (ts);
ALTER TABLE whoopsy_band ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON whoopsy_band
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, DELETE ON whoopsy_band TO monitor_system;
GRANT SELECT ON whoopsy_band TO monitor_app;

-- +goose Down
DROP TABLE whoopsy_band;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION usage_close_hour(p_start timestamptz, p_revision integer, p_reason text) RETURNS integer
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
        SELECT tenant_id, plugin, whoopsy,
               sum(extract(epoch FROM least(coalesce(ended_at, p_end), p_end) - greatest(started_at, p_start)))::bigint AS s
          FROM public.check_periods
         WHERE started_at < p_end AND coalesce(ended_at, 'infinity') > p_start
         GROUP BY 1, 2, 3
    )
    -- A check with Whoopsy! is billed as "<plugin>+whoopsy": its own weight
    -- when the config sets one, else the plugin's weight times the
    -- multiplier "*whoopsy" (default 5).
    SELECT s.tenant_id, s.plugin || CASE WHEN s.whoopsy THEN '+whoopsy' ELSE '' END AS plugin, s.s AS count_seconds,
           CASE WHEN s.whoopsy THEN
               coalesce((SELECT weight FROM w WHERE w.plugin = s.plugin || '+whoopsy'),
                        coalesce((SELECT weight FROM w WHERE w.plugin = s.plugin),
                                 (SELECT weight FROM w WHERE w.plugin = '*'), 1)
                        * coalesce((SELECT weight FROM w WHERE w.plugin = '*whoopsy'), 5))
           ELSE
               coalesce((SELECT weight FROM w WHERE w.plugin = s.plugin),
                        (SELECT weight FROM w WHERE w.plugin = '*'), 1)
           END AS weight
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
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION usage_sync_check(p_check uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE
    c_tenant   uuid;
    c_device   uuid;
    c_plugin   text;
    c_interval integer;
    c_whoopsy  boolean;
    c_billable boolean := false;
    o_id       bigint;
    o_device   uuid;
    o_plugin   text;
    o_interval integer;
    o_whoopsy  boolean;
BEGIN
    SELECT ch.tenant_id, ch.device_id, ch.plugin, ch.interval_seconds, ch.whoopsy IS NOT NULL,
           ch.enabled AND t.status = 'active' AND NOT t.is_platform
      INTO c_tenant, c_device, c_plugin, c_interval, c_whoopsy, c_billable
      FROM public.checks ch JOIN public.tenants t ON t.id = ch.tenant_id
     WHERE ch.id = p_check;
    c_billable := coalesce(c_billable, false);
    SELECT id, device_id, plugin, interval_seconds, whoopsy INTO o_id, o_device, o_plugin, o_interval, o_whoopsy
      FROM public.check_periods WHERE check_id = p_check AND ended_at IS NULL FOR UPDATE;
    IF o_id IS NOT NULL AND (NOT c_billable OR o_plugin <> c_plugin OR o_interval <> c_interval
            OR o_device <> c_device OR o_whoopsy <> c_whoopsy) THEN
        UPDATE public.check_periods SET ended_at = greatest(now(), started_at) WHERE id = o_id;
        o_id := NULL;
    END IF;
    IF o_id IS NULL AND c_billable THEN
        INSERT INTO public.check_periods (tenant_id, check_id, device_id, plugin, interval_seconds, whoopsy, started_at)
        VALUES (c_tenant, p_check, c_device, c_plugin, c_interval, c_whoopsy, now());
    END IF;
END
$$;
-- +goose StatementEnd
DROP FUNCTION usage_sync_check_objects(uuid);
DROP FUNCTION usage_sync_objects(uuid, boolean);
DROP TABLE object_periods;
ALTER TABLE check_objects DROP COLUMN billing_key;
