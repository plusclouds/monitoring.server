// Package usage meters billable checks (F13, milestone M3.5): it records
// plugin weights from the config, closes UTC hours into usage rows, and
// reads them for the API. Billable periods themselves are kept by database
// triggers (migration 00010).
package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/config"
)

// DefaultPlugin is the weight row of plugins without their own weight.
const DefaultPlugin = "*"

// WeightChange is a weight recorded by SyncWeights. A nil Weight means the
// plugin falls back to the default weight.
type WeightChange struct {
	Plugin        string
	From, To      *float64
	EffectiveFrom time.Time
}

// SyncWeights records the config's weights where they differ from the
// latest recorded ones, effective from the next full hour. A plugin seen
// for the first time applies from the beginning of time, so the first
// deploy is billed with the configured weights. Each change is audited in
// the platform tenant.
func SyncWeights(ctx context.Context, db *pgxpool.Pool, cfg config.Usage, now time.Time) ([]WeightChange, error) {
	want := map[string]*float64{DefaultPlugin: ptr(cfg.DefaultWeight)}
	for p, w := range cfg.Weights {
		want[p] = ptr(w)
	}
	var out []WeightChange
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('usage_weights'))`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT ON (plugin) plugin, weight::float8 FROM usage_weights
			ORDER BY plugin, effective_from DESC`)
		if err != nil {
			return err
		}
		have := map[string]*float64{}
		var p string
		var w *float64
		if _, err := pgx.ForEachRow(rows, []any{&p, &w}, func() error { have[p] = w; return nil }); err != nil {
			return err
		}
		for p := range have {
			if _, ok := want[p]; !ok && have[p] != nil {
				want[p] = nil // removed from the config: back to the default
			}
		}
		next := now.UTC().Truncate(time.Hour).Add(time.Hour)
		plugins := make([]string, 0, len(want))
		for p := range want {
			plugins = append(plugins, p)
		}
		slices.Sort(plugins)
		for _, p := range plugins {
			cur, seen := have[p]
			if seen && equal(cur, want[p]) {
				continue
			}
			from := next
			if !seen {
				from = time.Time{} // stored as -infinity
			}
			fromArg := any(from)
			if from.IsZero() {
				fromArg = pgInfinity
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO usage_weights (plugin, effective_from, weight) VALUES ($1, $2, $3)
				ON CONFLICT (plugin, effective_from) DO UPDATE SET weight = excluded.weight, recorded_at = now()`,
				p, fromArg, want[p]); err != nil {
				return err
			}
			out = append(out, WeightChange{Plugin: p, From: cur, To: want[p], EffectiveFrom: from})
		}
		if len(out) == 0 {
			return nil
		}
		var platform uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM tenants WHERE is_platform`).Scan(&platform)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not bootstrapped yet: nothing to audit against
		}
		if err != nil {
			return err
		}
		for _, c := range out {
			if err := audit.Write(ctx, tx, audit.Event{TenantID: platform, ActorKind: audit.ActorFile,
				ActorDetail: "usage.weights", Action: "usage.weight.update", ObjectType: "usage_weight",
				ObjectID: c.Plugin, Before: map[string]any{"weight": c.From},
				After: map[string]any{"weight": c.To, "effective_from": effective(c.EffectiveFrom)}}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// pgInfinity is '-infinity': the weight applies to every hour.
var pgInfinity = pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}

func ptr(v float64) *float64 { return &v }

func equal(a, b *float64) bool {
	return (a == nil) == (b == nil) && (a == nil || math.Abs(*a-*b) < 0.0005)
}

// Effective is when the change applies, or "always" for a plugin's first weight.
func (c WeightChange) Effective() string { return effective(c.EffectiveFrom) }

func effective(t time.Time) string {
	if t.IsZero() {
		return "always"
	}
	return t.Format(time.RFC3339)
}

// CloseHours closes every UTC hour that ended more than delay ago and is
// not closed yet, in order, one transaction per hour. It returns the hours
// closed.
func CloseHours(ctx context.Context, db *pgxpool.Pool, delay time.Duration, now time.Time) ([]time.Time, error) {
	var done []time.Time
	for {
		var closed bool
		var hour time.Time
		err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `SELECT closed_until FROM usage_close FOR UPDATE`).Scan(&hour)
			if errors.Is(err, pgx.ErrNoRows) {
				// Fresh database: counting starts with the current hour.
				_, err = tx.Exec(ctx, `INSERT INTO usage_close (closed_until) VALUES ($1) ON CONFLICT DO NOTHING`,
					now.UTC().Truncate(time.Hour))
				return err
			}
			if err != nil || hour.Add(time.Hour+delay).After(now) {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT usage_close_hour($1, 1, NULL)`, hour); err != nil {
				return fmt.Errorf("close %s: %w", hour.UTC().Format(time.RFC3339), err)
			}
			_, err = tx.Exec(ctx, `UPDATE usage_close SET closed_until = $1`, hour.Add(time.Hour))
			closed = err == nil
			return err
		})
		if err != nil || !closed {
			return done, err
		}
		done = append(done, hour.UTC())
	}
}

// ClosedUntil returns the end of the closed hours.
func ClosedUntil(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (time.Time, error) {
	var t time.Time
	err := q.QueryRow(ctx, `SELECT closed_until FROM usage_close`).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t.UTC(), err
}

// Recompute writes a new revision of every closed hour in [from, to) with
// a reason, for corrections. It returns the hours rewritten.
func Recompute(ctx context.Context, db *pgxpool.Pool, from, to time.Time, reason string) ([]time.Time, error) {
	if reason == "" {
		return nil, errors.New("a reason is required")
	}
	until, err := ClosedUntil(ctx, db)
	if err != nil {
		return nil, err
	}
	var done []time.Time
	for h := from.UTC().Truncate(time.Hour); h.Before(to) && h.Before(until); h = h.Add(time.Hour) {
		err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			var rev int
			if err := tx.QueryRow(ctx, `SELECT coalesce(max(revision), 1) + 1 FROM usage_hours WHERE period_start = $1`, h).
				Scan(&rev); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT usage_close_hour($1, $2, $3)`, h, rev, reason)
			return err
		})
		if err != nil {
			return done, err
		}
		done = append(done, h)
	}
	return done, nil
}

// ApplyRetention deletes usage hours older than keep.
func ApplyRetention(ctx context.Context, db *pgxpool.Pool, keep time.Duration) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	tag, err := db.Exec(ctx, `DELETE FROM usage_hours WHERE period_start < now() - $1::interval`, keep)
	return tag.RowsAffected(), err
}
