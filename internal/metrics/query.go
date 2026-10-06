package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Series is one stored metric of a check. A plugin layout change or a
// retention class change starts a new series with the same name.
type Series struct {
	ID       int64
	DeviceID uuid.UUID
	CheckID  uuid.UUID
	Plugin   string
	Object   string
	Name     string
	Unit     string
	Kind     string
	Class    string

	classID int16
	groupID int64
	slot    int
}

// Selector picks series. At least a device or a check is required.
type Selector struct {
	DeviceID *uuid.UUID
	CheckID  *uuid.UUID
	Names    []string
	Object   *string
}

// MaxSeries is the most series one selector may match.
const MaxSeries = 200

// ListSeries returns the series of a selector, in a transaction scoped to
// the tenant (RLS).
func ListSeries(ctx context.Context, tx pgx.Tx, sel Selector) ([]Series, error) {
	if sel.DeviceID == nil && sel.CheckID == nil {
		return nil, errs.Invalidf("device_id", "a device_id or a check_id is required")
	}
	where := []string{"true"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if sel.DeviceID != nil {
		add("g.device_id = $%d", *sel.DeviceID)
	}
	if sel.CheckID != nil {
		add("g.source_id = $%d", *sel.CheckID)
	}
	if len(sel.Names) > 0 {
		add("s.name = ANY($%d)", sel.Names)
	}
	if sel.Object != nil {
		add("g.object_key = $%d", *sel.Object)
	}
	rows, err := tx.Query(ctx, `
		SELECT s.id, g.device_id, g.source_id, g.plugin, g.object_key, s.name, s.unit, s.kind, rc.name,
		       g.retention_class, g.id, s.slot
		  FROM metric_series s
		  JOIN metric_groups g ON g.id = s.group_id
		  JOIN retention_classes rc ON rc.id = g.retention_class
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY g.device_id, g.source_id, g.object_key, s.name, s.id
		 LIMIT `+fmt.Sprint(MaxSeries+1), args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Series, error) {
		var s Series
		err := r.Scan(&s.ID, &s.DeviceID, &s.CheckID, &s.Plugin, &s.Object, &s.Name, &s.Unit, &s.Kind, &s.Class,
			&s.classID, &s.groupID, &s.slot)
		return s, err
	})
	if err != nil {
		return nil, err
	}
	if len(out) > MaxSeries {
		return nil, errs.Invalidf("name", "the selector matches more than %d series; narrow it", MaxSeries)
	}
	return out, nil
}

// Aggregations of a query.
const (
	AggAvg = "avg"
	AggMin = "min"
	AggMax = "max"
	AggSum = "sum"
)

// Query reads points of the selected series.
type Query struct {
	Selector
	From, To   time.Time
	Step       time.Duration // 0 picks about 500 points
	Agg        string        // default avg
	Resolution string        // raw, 5m or 1h; empty picks from the step and retention
	MaxPoints  int
	// MovingWindow, above 1, replaces every point with the mean of the
	// last MovingWindow points.
	MovingWindow int
}

// Point is one bucket of a series.
type Point struct {
	T time.Time
	V float64
}

// Result is a query's series, merged by (check, object, name).
type Result struct {
	Resolution string
	Step       time.Duration
	Series     []QuerySeries
}

// QuerySeries is one metric with its points.
type QuerySeries struct {
	DeviceID  uuid.UUID
	CheckID   uuid.UUID
	Object    string
	Name      string
	Unit      string
	SeriesIDs []int64
	Points    []Point
}

type bucket struct {
	min, max, sum float64
	count         int64
}

var levelSize = map[string]time.Duration{LevelRaw: 0, Level5m: 5 * time.Minute, Level1h: time.Hour}

// origin aligns buckets: they start at multiples of the step since then, so
// the same step gives the same buckets whatever From is.
var origin = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Run executes the query in a transaction scoped to the tenant. Recent data the rollups have not reached yet is read
// from the finer level below, so a long-range graph never ends early.
func Run(ctx context.Context, tx pgx.Tx, q Query) (Result, error) {
	if err := q.normalize(); err != nil {
		return Result{}, err
	}
	series, err := ListSeries(ctx, tx, q.Selector)
	if err != nil {
		return Result{}, err
	}
	if rawAgg(q.Agg) {
		return q.runRaw(ctx, tx, series)
	}
	level, err := q.level(ctx, tx, series)
	if err != nil {
		return Result{}, err
	}
	out := Result{Resolution: level, Step: q.Step}
	type key struct {
		check  uuid.UUID
		object string
		name   string
	}
	merged := map[key]int{}
	buckets := map[int]map[time.Time]*bucket{}
	for _, s := range series {
		k := key{s.CheckID, s.Object, s.Name}
		i, ok := merged[k]
		if !ok {
			i = len(out.Series)
			merged[k] = i
			out.Series = append(out.Series, QuerySeries{DeviceID: s.DeviceID, CheckID: s.CheckID, Object: s.Object,
				Name: s.Name, Unit: s.Unit})
			buckets[i] = map[time.Time]*bucket{}
		}
		out.Series[i].SeriesIDs = append(out.Series[i].SeriesIDs, s.ID)
		if err := q.read(ctx, tx, s, level, buckets[i]); err != nil {
			return Result{}, err
		}
	}
	for i := range out.Series {
		out.Series[i].Points = MovingAverage(points(buckets[i], q.Agg), q.MovingWindow)
	}
	return out, nil
}

// runRaw answers percentile and standard deviation queries from raw
// samples, whatever the step.
func (q *Query) runRaw(ctx context.Context, tx pgx.Tx, series []Series) (Result, error) {
	if q.Resolution != "" && q.Resolution != LevelRaw {
		return Result{}, errs.Invalidf("resolution", "%s is computed from raw samples only", q.Agg)
	}
	if err := rawCovers(ctx, tx, series, q.From); err != nil {
		return Result{}, err
	}
	out := Result{Resolution: LevelRaw, Step: q.Step}
	type key struct {
		check  uuid.UUID
		object string
		name   string
	}
	merged := map[key]int{}
	var samples [][]rawSample
	budget := 0
	for _, s := range series {
		k := key{s.CheckID, s.Object, s.Name}
		i, ok := merged[k]
		if !ok {
			i = len(out.Series)
			merged[k] = i
			out.Series = append(out.Series, QuerySeries{DeviceID: s.DeviceID, CheckID: s.CheckID, Object: s.Object,
				Name: s.Name, Unit: s.Unit})
			samples = append(samples, nil)
		}
		out.Series[i].SeriesIDs = append(out.Series[i].SeriesIDs, s.ID)
		raw, err := readRaw(ctx, tx, s, q.From, q.To, &budget)
		if err != nil {
			return Result{}, err
		}
		samples[i] = append(samples[i], raw...)
	}
	for i := range out.Series {
		out.Series[i].Points = MovingAverage(rawPoints(samples[i], q.Step, q.Agg), q.MovingWindow)
	}
	return out, nil
}

func (q *Query) normalize() error {
	if q.To.IsZero() {
		q.To = time.Now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-time.Hour)
	}
	if !q.From.Before(q.To) {
		return errs.Invalidf("from", "must be before to")
	}
	span := q.To.Sub(q.From)
	if q.Step == 0 {
		q.Step = max((span / 500).Round(time.Second), 10*time.Second)
	}
	if q.Step < time.Second {
		return errs.Invalidf("step", "must be at least 1s")
	}
	if q.MaxPoints <= 0 {
		q.MaxPoints = 10000
	}
	if n := span / q.Step; n > time.Duration(q.MaxPoints) {
		return errs.Invalidf("step", "%d points per series; at most %d (use a larger step)", n, q.MaxPoints)
	}
	if q.Agg == "" {
		q.Agg = AggAvg
	}
	if !slices.Contains([]string{AggAvg, AggMin, AggMax, AggSum}, q.Agg) && !rawAgg(q.Agg) {
		return errs.Invalidf("agg", "must be avg, min, max, sum, stddev or a percentile from p0.001 to p99.999 such as p95 or p99.9")
	}
	if q.MovingWindow < 0 || q.MovingWindow > 1000 {
		return errs.Invalidf("moving_window", "must be between 1 and 1000 points")
	}
	if _, ok := levelSize[q.Resolution]; q.Resolution != "" && !ok {
		return errs.Invalidf("resolution", "must be raw, 5m or 1h")
	}
	return nil
}

// level picks the resolution: the coarsest level not larger than the step,
// moved to a coarser level when the finer one no longer covers From.
func (q *Query) level(ctx context.Context, tx pgx.Tx, series []Series) (string, error) {
	if q.Resolution != "" {
		return q.Resolution, nil
	}
	level := LevelRaw
	switch {
	case q.Step >= time.Hour:
		level = Level1h
	case q.Step >= 5*time.Minute:
		level = Level5m
	}
	classes := map[int16]bool{}
	for _, s := range series {
		classes[s.classID] = true
	}
	keep := map[string]int{} // shortest retention per level among the classes
	rows, err := tx.Query(ctx, `SELECT class_id, level, keep_days FROM retention_policies`)
	if err != nil {
		return "", err
	}
	var class int16
	var lvl string
	var days int
	if _, err := pgx.ForEachRow(rows, []any{&class, &lvl, &days}, func() error {
		if classes[class] && (keep[lvl] == 0 || days < keep[lvl]) {
			keep[lvl] = days
		}
		return nil
	}); err != nil {
		return "", err
	}
	covers := func(l string) bool {
		d, ok := keep[l]
		return !ok || !q.From.Before(time.Now().AddDate(0, 0, -d))
	}
	for _, l := range []string{LevelRaw, Level5m} {
		if level == l && !covers(l) {
			level = map[string]string{LevelRaw: Level5m, Level5m: Level1h}[l]
		}
	}
	return level, nil
}

// read adds the points of one series from level, and from the finer levels
// for the part after level's watermark.
func (q *Query) read(ctx context.Context, tx pgx.Tx, s Series, level string, into map[time.Time]*bucket) error {
	cur := q.From
	order := []string{Level1h, Level5m, LevelRaw}
	for _, l := range order[slices.Index(order, level):] {
		until := q.To
		if l != LevelRaw {
			var wm time.Time
			err := tx.QueryRow(ctx, `SELECT done_until FROM metric_rollup_watermarks WHERE level = $1 AND retention_class = $2`,
				l, s.classID).Scan(&wm)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if wm.Before(until) {
				until = wm
			}
		}
		if !cur.Before(until) {
			continue
		}
		var rows pgx.Rows
		var err error
		if l == LevelRaw {
			rows, err = tx.Query(ctx, `
				SELECT date_bin($4, m.ts, $7), min(x.v), max(x.v), sum(x.v), count(*)
				  FROM metric_samples m CROSS JOIN LATERAL (SELECT m.vals[$5::int] AS v) x
				 WHERE m.retention_class = $6 AND m.group_id = $1 AND m.ts >= $2 AND m.ts < $3
				   AND x.v <> 'NaN'::float8
				 GROUP BY 1`, s.groupID, cur, until, q.Step, s.slot, s.classID, origin)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT date_bin($4, ts, $6), min(min), max(max), sum(sum), sum(count)
				  FROM metric_rollup_`+l+`
				 WHERE retention_class = $5 AND series_id = $1 AND ts >= $2 AND ts < $3
				 GROUP BY 1`, s.ID, cur, until, q.Step, s.classID, origin)
		}
		if err != nil {
			return err
		}
		var t time.Time
		var b bucket
		if _, err := pgx.ForEachRow(rows, []any{&t, &b.min, &b.max, &b.sum, &b.count}, func() error {
			if have, ok := into[t]; ok {
				have.min, have.max = math.Min(have.min, b.min), math.Max(have.max, b.max)
				have.sum += b.sum
				have.count += b.count
			} else {
				nb := b
				into[t] = &nb
			}
			return nil
		}); err != nil {
			return err
		}
		cur = until
	}
	return nil
}

func points(m map[time.Time]*bucket, agg string) []Point {
	out := make([]Point, 0, len(m))
	for t, b := range m {
		v := b.sum / float64(b.count)
		switch agg {
		case AggMin:
			v = b.min
		case AggMax:
			v = b.max
		case AggSum:
			v = b.sum
		}
		out = append(out, Point{T: t.UTC(), V: v})
	}
	slices.SortFunc(out, func(a, b Point) int { return a.T.Compare(b.T) })
	return out
}
