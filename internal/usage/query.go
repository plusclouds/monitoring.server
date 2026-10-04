package usage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Hour is one tenant's closed hour, latest revision.
type Hour struct {
	AccountID            *string
	PeriodStart          time.Time
	Revision             int
	BillableCheckSeconds float64
	DeviceSeconds        int64
	Breakdown            map[string]PluginSeconds
}

// PluginSeconds is one plugin's share of an hour.
type PluginSeconds struct {
	CountSeconds int64   `json:"count_seconds"`
	Weight       float64 `json:"weight"`
}

// MaxRange is the longest range one request may read.
const MaxRange = 31 * 24 * time.Hour

// ErrOpen means the range reaches an hour that is not closed yet.
type ErrOpen struct{ ClosedUntil time.Time }

func (e *ErrOpen) Error() string {
	return "hours from " + e.ClosedUntil.Format(time.RFC3339) + " on are not closed yet"
}

// CheckRange validates a usage range against the closed hours.
func CheckRange(from, to, closedUntil time.Time) error {
	switch {
	case !from.Equal(from.Truncate(time.Hour)) || !to.Equal(to.Truncate(time.Hour)):
		return errs.Invalidf("from", "from and to must be whole UTC hours")
	case !from.Before(to):
		return errs.Invalidf("from", "must be before to")
	case to.Sub(from) > MaxRange:
		return errs.Invalidf("to", "a range covers at most 31 days")
	case to.After(closedUntil):
		return &ErrOpen{ClosedUntil: closedUntil}
	}
	return nil
}

// Cursor is the keyset position of a tenant-hours page.
type Cursor struct {
	Period  time.Time `json:"p"`
	Account string    `json:"a"`
}

// TenantHours returns billable hours of every tenant in [from, to), for
// the platform key, ordered by hour and account.
func TenantHours(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, from, to time.Time, after *Cursor, limit int) ([]Hour, error) {
	var ap *time.Time
	var aa *string
	if after != nil {
		ap, aa = &after.Period, &after.Account
	}
	rows, err := q.Query(ctx, `SELECT account_id, period_start, revision, billable_check_seconds::float8, device_seconds, breakdown
		FROM usage_tenant_hours($1, $2, $3, $4, $5)`, from, to, ap, aa, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// TenantOwnHours returns a tenant's own closed hours, including hours
// without billable checks, in a transaction scoped to the tenant.
func TenantOwnHours(ctx context.Context, tx pgx.Tx, from, to time.Time) ([]Hour, error) {
	rows, err := tx.Query(ctx, `
		SELECT h.account_id, h.period_start, h.revision, h.billable_check_seconds::float8, h.device_seconds,
		       coalesce((SELECT jsonb_object_agg(p.plugin, jsonb_build_object('count_seconds', p.count_seconds, 'weight', p.weight))
		                   FROM usage_hour_plugins p
		                  WHERE p.tenant_id = h.tenant_id AND p.period_start = h.period_start AND p.revision = h.revision), '{}')
		  FROM (SELECT DISTINCT ON (period_start) * FROM usage_hours
		         WHERE period_start >= $1 AND period_start < $2 ORDER BY period_start, revision DESC) h
		 ORDER BY h.period_start`, from, to)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

func collect(rows pgx.Rows) ([]Hour, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hour, error) {
		var h Hour
		var b []byte
		if err := r.Scan(&h.AccountID, &h.PeriodStart, &h.Revision, &h.BillableCheckSeconds, &h.DeviceSeconds, &b); err != nil {
			return h, err
		}
		h.PeriodStart = h.PeriodStart.UTC()
		return h, json.Unmarshal(b, &h.Breakdown)
	})
}

// Weights returns the weights in force at t, and the default weight.
func Weights(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, t time.Time) (map[string]float64, float64, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (plugin) plugin, weight::float8 FROM usage_weights
		WHERE effective_from <= $1 ORDER BY plugin, effective_from DESC`, t)
	if err != nil {
		return nil, 0, err
	}
	out := map[string]float64{}
	def := 1.0
	var p string
	var w *float64
	_, err = pgx.ForEachRow(rows, []any{&p, &w}, func() error {
		switch {
		case p == DefaultPlugin:
			def = *w
		case w != nil:
			out[p] = *w
		}
		return nil
	})
	return out, def, err
}

// Current is what a tenant is billed for right now.
type Current struct {
	Checks         int
	WeightedChecks float64
	ByPlugin       map[string]PluginCount
}

// PluginCount is the number of billable checks of one plugin now.
type PluginCount struct {
	Checks int     `json:"checks"`
	Weight float64 `json:"weight"`
}

// CurrentUsage counts the tenant's open check periods, weighted, in a
// transaction scoped to the tenant.
func CurrentUsage(ctx context.Context, tx pgx.Tx, now time.Time) (Current, error) {
	weights, def, err := Weights(ctx, tx, now)
	if err != nil {
		return Current{}, err
	}
	rows, err := tx.Query(ctx, `SELECT plugin, count(*) FROM check_periods WHERE ended_at IS NULL GROUP BY 1`)
	if err != nil {
		return Current{}, err
	}
	out := Current{ByPlugin: map[string]PluginCount{}}
	var p string
	var n int
	_, err = pgx.ForEachRow(rows, []any{&p, &n}, func() error {
		w, ok := weights[p]
		if !ok {
			w = def
		}
		out.ByPlugin[p] = PluginCount{Checks: n, Weight: w}
		out.Checks += n
		out.WeightedChecks += float64(n) * w
		return nil
	})
	return out, err
}
