-- Whoopsy! (premium alerting): a check's moving-average band rule, and its
-- billing as "<plugin>+whoopsy" at the plugin's weight times a multiplier.

-- +goose Up

-- The Whoopsy! settings of a check; null when it is off.
ALTER TABLE checks ADD COLUMN whoopsy jsonb CHECK (whoopsy IS NULL OR jsonb_typeof(whoopsy) = 'object');

ALTER TABLE check_periods ADD COLUMN whoopsy boolean NOT NULL DEFAULT false;

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

DROP TRIGGER usage_periods ON checks;
CREATE TRIGGER usage_periods AFTER INSERT OR DELETE OR UPDATE OF enabled, plugin, interval_seconds, device_id, whoopsy ON checks
    FOR EACH ROW EXECUTE FUNCTION usage_on_check();

-- +goose Down
DROP TRIGGER usage_periods ON checks;
CREATE TRIGGER usage_periods AFTER INSERT OR DELETE OR UPDATE OF enabled, plugin, interval_seconds, device_id ON checks
    FOR EACH ROW EXECUTE FUNCTION usage_on_check();
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
-- +goose StatementEnd
ALTER TABLE check_periods DROP COLUMN whoopsy;
ALTER TABLE checks DROP COLUMN whoopsy;
