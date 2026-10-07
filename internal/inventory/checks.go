package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/credential"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/incident"
	"github.com/plusclouds/monitoring.server/internal/threshold"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Check is one plugin run against a device on an interval.
type Check struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	DeviceID          uuid.UUID
	Name              string
	Plugin            string
	Config            json.RawMessage
	IntervalSeconds   int
	TimeoutSeconds    *int
	Enabled           bool
	Thresholds        []threshold.Rule
	FailureCount      int
	RecoveryCount     int
	IsHostCheck       bool
	UnknownIsCritical bool
	RunbookURL        *string
	Credentials       map[string]uuid.UUID // role -> credential
	Whoopsy           *threshold.Whoopsy   // premium band alerting; nil when off
	Push              *PushSource          // push checks (F08) only
	PushToken         string               // the new ingest token, only right after create or rotate
	ManagedBy         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CheckInput is the client-writable part of a check. Updates replace it;
// zero values mean the plugin's or the engine's default.
type CheckInput struct {
	Name              string
	Plugin            string
	Config            json.RawMessage
	IntervalSeconds   int // 0 = the plugin's default interval
	TimeoutSeconds    *int
	Enabled           bool
	Thresholds        []threshold.Rule
	FailureCount      int // 0 = 3
	RecoveryCount     int // 0 = 1
	IsHostCheck       bool
	UnknownIsCritical bool
	RunbookURL        *string
	Credentials       map[string]uuid.UUID
}

// Limits the tenant puts on checks (F01).
type Limits struct {
	MaxChecks               int
	MinCheckIntervalSeconds int
}

var roleRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// prepare validates the input against the plugin and the tenant limits and
// fills in defaults.
func prepare(ctx context.Context, tx pgx.Tx, in CheckInput, lim Limits) (CheckInput, error) {
	if err := name("name", in.Name); err != nil {
		return in, err
	}
	p, ok := plugin.Lookup(in.Plugin)
	if !ok {
		return in, errs.Invalidf("plugin", "unknown plugin %q; see GET /v1/plugins", in.Plugin)
	}
	m := p.Manifest()
	if len(in.Config) == 0 || string(in.Config) == "null" {
		in.Config = json.RawMessage("{}")
	}
	if err := plugin.ValidateConfig(p, in.Config); err != nil {
		return in, errs.Invalidf("config", "%s", err.Error())
	}
	if in.IntervalSeconds == 0 {
		in.IntervalSeconds = max(int(m.DefaultInterval/time.Second), lim.MinCheckIntervalSeconds)
	}
	minInterval := max(int(m.MinInterval/time.Second), lim.MinCheckIntervalSeconds)
	if in.IntervalSeconds < minInterval {
		return in, errs.Invalidf("interval_seconds", "must be at least %d for %s in this tenant", minInterval, m.Type)
	}
	if in.IntervalSeconds > 86400 {
		return in, errs.Invalidf("interval_seconds", "must be at most 86400")
	}
	if t := in.TimeoutSeconds; t != nil && (*t < 1 || *t > 300 || *t > in.IntervalSeconds) {
		return in, errs.Invalidf("timeout_seconds", "must be between 1 and min(300, interval_seconds)")
	}
	if in.FailureCount == 0 {
		in.FailureCount = 3
		if m.Kind == plugin.KindIngester {
			in.FailureCount = 1 // missed_count already waits for the silence to last
		}
	}
	if in.RecoveryCount == 0 {
		in.RecoveryCount = 1
	}
	if in.FailureCount < 1 || in.FailureCount > 100 || in.RecoveryCount < 1 || in.RecoveryCount > 100 {
		return in, errs.Invalidf("failure_count", "failure_count and recovery_count must be between 1 and 100")
	}
	if in.RunbookURL != nil {
		u, err := url.Parse(*in.RunbookURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(*in.RunbookURL) > 2000 {
			return in, errs.Invalidf("runbook_url", "must be an http(s) URL")
		}
	}
	if m.Kind == plugin.KindCollector && in.IsHostCheck {
		return in, errs.Invalidf("is_host_check", "a collector (%s) cannot be the host check; use a ping or TCP check", m.Type)
	}
	defs := plugin.MetricsOf(p, in.Config)
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	for i, r := range in.Thresholds {
		if r.Object != "" && m.Kind != plugin.KindCollector {
			return in, errs.Invalidf(fmt.Sprintf("thresholds[%d].object", i), "only collectors report objects; %s is a check", m.Type)
		}
	}
	rules, err := threshold.Validate(in.Thresholds, names)
	if err != nil {
		return in, err
	}
	in.Thresholds = rules
	for role, id := range in.Credentials {
		if !roleRe.MatchString(role) {
			return in, errs.Invalidf("credentials", "role %q must be lowercase letters, digits and _", role)
		}
		c, err := credential.Get(ctx, tx, id)
		if errors.Is(err, errs.ErrNotFound) {
			return in, errs.Invalidf("credentials."+role, "credential %s does not exist", id)
		}
		if err != nil {
			return in, err
		}
		if !slices.Contains(m.CredentialTypes, c.Type) {
			return in, errs.Invalidf("credentials."+role, "%s does not accept %s credentials; it accepts %v", m.Type, c.Type, m.CredentialTypes)
		}
	}
	return in, nil
}

const checkCols = `c.id, c.tenant_id, c.device_id, c.name, c.plugin, c.config, c.interval_seconds, c.timeout_seconds,
	c.enabled, c.thresholds, c.failure_count, c.recovery_count, c.is_host_check, c.unknown_is_critical,
	c.runbook_url, c.managed_by, c.created_at, c.updated_at, c.whoopsy,
	coalesce((SELECT jsonb_object_agg(cc.role, cc.credential_id) FROM check_credentials cc WHERE cc.check_id = c.id), '{}'),
	(SELECT jsonb_build_object('token_prefix', ps.token_prefix, 'token_created_at', ps.token_created_at,
	        'last_push_at', ps.last_push_at) FROM push_sources ps WHERE ps.check_id = c.id)`

var checkConstraints = map[string]string{
	"checks_device_id_name_key": "the device already has a check with this name",
	"checks_one_host_check":     "the device already has a host check",
}

func scanCheck(row pgx.Row) (Check, error) {
	var c Check
	var creds map[string]uuid.UUID
	err := row.Scan(&c.ID, &c.TenantID, &c.DeviceID, &c.Name, &c.Plugin, &c.Config, &c.IntervalSeconds,
		&c.TimeoutSeconds, &c.Enabled, &c.Thresholds, &c.FailureCount, &c.RecoveryCount, &c.IsHostCheck,
		&c.UnknownIsCritical, &c.RunbookURL, &c.ManagedBy, &c.CreatedAt, &c.UpdatedAt, &c.Whoopsy, &creds, &c.Push)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errs.ErrNotFound
	}
	c.Credentials = creds
	return c, err
}

func (c Check) snapshot() map[string]any {
	return map[string]any{
		"device_id": c.DeviceID, "name": c.Name, "plugin": c.Plugin, "config": c.Config,
		"interval_seconds": c.IntervalSeconds, "timeout_seconds": c.TimeoutSeconds, "enabled": c.Enabled,
		"thresholds": c.Thresholds, "failure_count": c.FailureCount, "recovery_count": c.RecoveryCount,
		"is_host_check": c.IsHostCheck, "unknown_is_critical": c.UnknownIsCritical,
		"runbook_url": c.RunbookURL, "credentials": c.Credentials, "whoopsy": c.Whoopsy,
	}
}

// SetWhoopsy turns Whoopsy! on (with settings) or off (nil) for a check.
// It is separate from UpdateCheck, so a client replacing a check without
// knowing Whoopsy! never turns it off (it changes billing).
func SetWhoopsy(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, w *threshold.Whoopsy) (Check, error) {
	cur, err := scanCheck(tx.QueryRow(ctx, `SELECT `+checkCols+` FROM checks c WHERE c.id = $1 FOR UPDATE`, id))
	if err != nil {
		return Check{}, err
	}
	if w != nil {
		p, ok := plugin.Lookup(cur.Plugin)
		if !ok {
			return Check{}, errs.Invalidf("plugin", "plugin %q is not built into this engine", cur.Plugin)
		}
		m := p.Manifest()
		if m.Kind != plugin.KindCheck {
			return Check{}, errs.Invalidf("whoopsy", "Whoopsy! watches a polled check's metric; %s is a %s", m.Type, m.Kind)
		}
		names := make([]string, len(m.Metrics))
		for i, d := range m.Metrics {
			names[i] = d.Name
		}
		v, err := threshold.ValidateWhoopsy(*w, names, m.WhoopsyMetric)
		if err != nil {
			return Check{}, err
		}
		w = &v
	}
	if reflect.DeepEqual(cur.Whoopsy, w) {
		return cur, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE checks SET whoopsy = $2, updated_at = now() WHERE id = $1`, id, w); err != nil {
		return Check{}, err
	}
	action := "check.whoopsy.enable"
	switch {
	case w == nil:
		action = "check.whoopsy.disable"
	case cur.Whoopsy != nil:
		action = "check.whoopsy.update"
	}
	out, err := GetCheck(ctx, tx, id)
	if err != nil {
		return Check{}, err
	}
	return out, audit.Write(ctx, tx, actor.Event(cur.TenantID, action, "check", id.String(),
		map[string]any{"whoopsy": cur.Whoopsy}, map[string]any{"whoopsy": w}))
}

// GetCheck returns one check.
func GetCheck(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Check, error) {
	return scanCheck(tx.QueryRow(ctx, `SELECT `+checkCols+` FROM checks c WHERE c.id = $1`, id))
}

// LockCheck reads a check for update (PATCH).
func LockCheck(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Check, error) {
	return scanCheck(tx.QueryRow(ctx, `SELECT `+checkCols+` FROM checks c WHERE c.id = $1 FOR UPDATE OF c`, id))
}

// CheckFilter narrows ListChecks.
type CheckFilter struct {
	DeviceID *uuid.UUID
	Plugin   *string
	Enabled  *bool
}

// ListChecks returns checks ordered by ID.
func ListChecks(ctx context.Context, tx pgx.Tx, f CheckFilter, p Page) ([]Check, error) {
	rows, err := tx.Query(ctx, `SELECT `+checkCols+` FROM checks c
		 WHERE ($1::uuid IS NULL OR c.device_id = $1)
		   AND ($2::text IS NULL OR c.plugin = $2)
		   AND ($3::boolean IS NULL OR c.enabled = $3)
		   AND ($4::uuid IS NULL OR c.id > $4)
		 ORDER BY c.id LIMIT $5`, f.DeviceID, f.Plugin, f.Enabled, p.After, p.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Check, error) { return scanCheck(r) })
}

// CreateCheck adds a check to a device within the tenant's max_checks limit.
func CreateCheck(ctx context.Context, tx pgx.Tx, actor audit.Actor, device uuid.UUID, lim Limits, in CheckInput) (Check, error) {
	d, err := GetDevice(ctx, tx, device)
	if err != nil {
		return Check{}, err
	}
	if in, err = prepare(ctx, tx, in, lim); err != nil {
		return Check{}, err
	}
	if err := lockTenant(ctx, tx, "checks", d.TenantID); err != nil {
		return Check{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM checks WHERE tenant_id = $1`, d.TenantID).Scan(&n); err != nil {
		return Check{}, err
	}
	if n >= lim.MaxChecks {
		return Check{}, errs.Conflictf("limit-reached", "the tenant has reached its limit of %d checks", lim.MaxChecks)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Check{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO checks (id, tenant_id, device_id, name, plugin, config, interval_seconds, timeout_seconds,
		                    enabled, thresholds, failure_count, recovery_count, is_host_check,
		                    unknown_is_critical, runbook_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		id, d.TenantID, device, in.Name, in.Plugin, in.Config, in.IntervalSeconds, in.TimeoutSeconds,
		in.Enabled, nonNilRules(in.Thresholds), in.FailureCount, in.RecoveryCount, in.IsHostCheck,
		in.UnknownIsCritical, in.RunbookURL)
	if err != nil {
		return Check{}, errs.FromDB(err, checkConstraints)
	}
	if err := setCredentials(ctx, tx, d.TenantID, id, in.Credentials); err != nil {
		return Check{}, err
	}
	var token string
	if p, _ := plugin.Lookup(in.Plugin); p != nil && p.Manifest().Kind == plugin.KindIngester {
		if token, err = setPushToken(ctx, tx, d.TenantID, id); err != nil {
			return Check{}, err
		}
	}
	c, err := GetCheck(ctx, tx, id)
	if err != nil {
		return Check{}, err
	}
	c.PushToken = token
	if n+1 == lim.MaxChecks {
		if err := incident.LimitReached(ctx, tx, d.TenantID, "checks", lim.MaxChecks, n+1); err != nil {
			return Check{}, err
		}
	}
	return c, audit.Write(ctx, tx, actor.Event(d.TenantID, "check.create", "check", id.String(), nil, c.snapshot()))
}

// UpdateCheck replaces a check's fields. The device and plugin cannot
// change; an update that changes nothing writes no audit event.
func UpdateCheck(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, lim Limits, in CheckInput) (Check, error) {
	cur, err := scanCheck(tx.QueryRow(ctx, `SELECT `+checkCols+` FROM checks c WHERE c.id = $1 FOR UPDATE`, id))
	if err != nil {
		return Check{}, err
	}
	if err := writable(cur.ManagedBy); err != nil {
		return Check{}, err
	}
	if in.Plugin == "" {
		in.Plugin = cur.Plugin
	}
	if in.Plugin != cur.Plugin {
		return Check{}, errs.Conflictf("plugin-change", "a check's plugin cannot change; create a new check")
	}
	if in, err = prepare(ctx, tx, in, lim); err != nil {
		return Check{}, err
	}
	next := cur
	next.Name, next.Config, next.IntervalSeconds, next.TimeoutSeconds = in.Name, in.Config, in.IntervalSeconds, in.TimeoutSeconds
	next.Enabled, next.Thresholds, next.FailureCount, next.RecoveryCount = in.Enabled, nonNilRules(in.Thresholds), in.FailureCount, in.RecoveryCount
	next.IsHostCheck, next.UnknownIsCritical, next.RunbookURL = in.IsHostCheck, in.UnknownIsCritical, in.RunbookURL
	next.Credentials = maps.Clone(in.Credentials)
	if next.Credentials == nil {
		next.Credentials = map[string]uuid.UUID{}
	}
	before, after := normalized(cur.snapshot()), normalized(next.snapshot())
	if reflect.DeepEqual(before, after) {
		return cur, nil
	}
	_, err = tx.Exec(ctx, `
		UPDATE checks SET name = $2, config = $3, interval_seconds = $4, timeout_seconds = $5, enabled = $6,
		       thresholds = $7, failure_count = $8, recovery_count = $9, is_host_check = $10,
		       unknown_is_critical = $11, runbook_url = $12, updated_at = now()
		 WHERE id = $1`,
		id, next.Name, next.Config, next.IntervalSeconds, next.TimeoutSeconds, next.Enabled, next.Thresholds,
		next.FailureCount, next.RecoveryCount, next.IsHostCheck, next.UnknownIsCritical, next.RunbookURL)
	if err != nil {
		return Check{}, errs.FromDB(err, checkConstraints)
	}
	if err := setCredentials(ctx, tx, cur.TenantID, id, next.Credentials); err != nil {
		return Check{}, err
	}
	if cur.Enabled && !next.Enabled {
		// A disabled check never recovers: end its incident now (F05).
		if err := resolveDisabled(ctx, tx, id); err != nil {
			return Check{}, err
		}
	}
	out, err := GetCheck(ctx, tx, id)
	if err != nil {
		return Check{}, err
	}
	return out, audit.Write(ctx, tx, actor.Event(cur.TenantID, "check.update", "check", id.String(), before, after))
}

// DeleteCheck removes a check.
func DeleteCheck(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID) error {
	cur, err := GetCheck(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := writable(cur.ManagedBy); err != nil {
		return err
	}
	if err := incident.ResolveForChecks(ctx, tx, `SELECT $1::uuid`, id); err != nil {
		return err
	}
	if err := deleteDiscovered(ctx, tx, actor, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM checks WHERE id = $1`, id); err != nil {
		return err
	}
	return audit.Write(ctx, tx, actor.Event(cur.TenantID, "check.delete", "check", id.String(), cur.snapshot(), nil))
}

func resolveDisabled(ctx context.Context, tx pgx.Tx, check uuid.UUID) error {
	if err := incident.ResolveForChecksBy(ctx, tx, "check-disabled", `SELECT $1::uuid`, check); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM check_state WHERE check_id = $1`, check); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM check_objects WHERE check_id = $1`, check)
	return err
}

func setCredentials(ctx context.Context, tx pgx.Tx, tenant, check uuid.UUID, creds map[string]uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM check_credentials WHERE check_id = $1`, check); err != nil {
		return err
	}
	for role, id := range creds {
		if _, err := tx.Exec(ctx, `INSERT INTO check_credentials (tenant_id, check_id, role, credential_id)
			VALUES ($1, $2, $3, $4)`, tenant, check, role, id); err != nil {
			return errs.FromDB(err, nil)
		}
	}
	return nil
}

func nonNilRules(r []threshold.Rule) []threshold.Rule {
	if r == nil {
		return []threshold.Rule{}
	}
	return r
}

// normalized round-trips a snapshot through JSON so that values read from
// the database and values built in Go compare equal (config key order,
// number types).
func normalized(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("snapshot is not JSON: %v", err))
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
