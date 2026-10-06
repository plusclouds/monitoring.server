package metrics

import (
	"context"
	"math"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Statistics that need every sample (percentiles, standard deviation) are
// computed from raw samples only; rollups keep min, max, sum and count.

// AggStdDev is the sample standard deviation of a bucket.
const AggStdDev = "stddev"

// MaxRawSamples bounds the raw samples one request reads.
const MaxRawSamples = 2_000_000

var percentileRe = regexp.MustCompile(`^p([0-9]{1,2}(\.[0-9]{1,3})?)$`)

// ParsePercentile reads "p95" or "p99.9" as 95 or 99.9. It reports false
// for anything else, including p0 and p100.
func ParsePercentile(agg string) (float64, bool) {
	m := percentileRe.FindStringSubmatch(agg)
	if m == nil {
		return 0, false
	}
	p, err := strconv.ParseFloat(m[1], 64)
	if err != nil || p <= 0 || p >= 100 {
		return 0, false
	}
	return p, true
}

// rawAgg reports whether an aggregation needs raw samples.
func rawAgg(agg string) bool {
	_, pct := ParsePercentile(agg)
	return pct || agg == AggStdDev
}

// Percentile of sorted values by linear interpolation between the closest
// ranks (PostgreSQL's percentile_cont, numpy's default).
func Percentile(sorted []float64, p float64) float64 {
	switch len(sorted) {
	case 0:
		return math.NaN()
	case 1:
		return sorted[0]
	}
	pos := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (pos-float64(lo))*(sorted[lo+1]-sorted[lo])
}

// MeanStdDev returns the mean and the sample standard deviation (n-1); the
// deviation is NaN for fewer than two values.
func MeanStdDev(v []float64) (mean, sd float64) {
	if len(v) == 0 {
		return math.NaN(), math.NaN()
	}
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	if len(v) < 2 {
		return mean, math.NaN()
	}
	var ss float64
	for _, x := range v {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss / float64(len(v)-1))
}

// sample is one raw value.
type rawSample struct {
	t time.Time
	v float64
}

// rawCovers fails unless raw retention of every class of the series covers
// from: these statistics are never computed from rollups.
func rawCovers(ctx context.Context, tx pgx.Tx, series []Series, from time.Time) error {
	classes := map[int16]bool{}
	for _, s := range series {
		classes[s.classID] = true
	}
	rows, err := tx.Query(ctx, `SELECT class_id, keep_days FROM retention_policies WHERE level = $1`, LevelRaw)
	if err != nil {
		return err
	}
	shortest := 0
	var class int16
	var days int
	if _, err := pgx.ForEachRow(rows, []any{&class, &days}, func() error {
		if classes[class] && (shortest == 0 || days < shortest) {
			shortest = days
		}
		return nil
	}); err != nil {
		return err
	}
	if shortest > 0 && from.Before(time.Now().AddDate(0, 0, -shortest)) {
		return errs.Invalidf("from", "percentiles and standard deviations are computed from raw samples, which are kept for %d days; from must be within them", shortest)
	}
	return nil
}

// readRaw returns the raw samples of a series in [from, to), oldest first,
// adding their number to *budget and failing when it passes MaxRawSamples.
func readRaw(ctx context.Context, tx pgx.Tx, s Series, from, to time.Time, budget *int) ([]rawSample, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.ts, x.v
		  FROM metric_samples m CROSS JOIN LATERAL (SELECT m.vals[$4::int] AS v) x
		 WHERE m.retention_class = $5 AND m.group_id = $1 AND m.ts >= $2 AND m.ts < $3 AND x.v <> 'NaN'::float8
		 ORDER BY m.ts`, s.groupID, from, to, s.slot, s.classID)
	if err != nil {
		return nil, err
	}
	var out []rawSample
	var r rawSample
	_, err = pgx.ForEachRow(rows, []any{&r.t, &r.v}, func() error {
		*budget++
		if *budget > MaxRawSamples {
			return errs.Invalidf("from", "the window holds more than %d raw samples; narrow it or select fewer metrics", MaxRawSamples)
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// rawPoints buckets raw samples by step and aggregates each bucket with a
// percentile or the standard deviation. Buckets without a value (a
// deviation of one sample) are left out.
func rawPoints(samples []rawSample, step time.Duration, agg string) []Point {
	byBucket := map[time.Time][]float64{}
	for _, s := range samples {
		b := origin.Add(s.t.Sub(origin) / step * step)
		byBucket[b] = append(byBucket[b], s.v)
	}
	p, isPct := ParsePercentile(agg)
	out := make([]Point, 0, len(byBucket))
	for t, vals := range byBucket {
		var v float64
		if isPct {
			slices.Sort(vals)
			v = Percentile(vals, p)
		} else {
			_, v = MeanStdDev(vals)
		}
		if math.IsNaN(v) {
			continue
		}
		out = append(out, Point{T: t.UTC(), V: v})
	}
	slices.SortFunc(out, func(a, b Point) int { return a.T.Compare(b.T) })
	return out
}

// MovingAverage replaces every point with the mean of itself and the
// window-1 points before it (fewer at the start of the series).
func MovingAverage(pts []Point, window int) []Point {
	if window <= 1 {
		return pts
	}
	out := make([]Point, len(pts))
	var sum float64
	for i, p := range pts {
		sum += p.V
		if i >= window {
			sum -= pts[i-window].V
		}
		n := min(i+1, window)
		out[i] = Point{T: p.T, V: sum / float64(n)}
	}
	return out
}

// SummaryQuery asks for whole-window statistics of the selected series.
type SummaryQuery struct {
	Selector
	From, To    time.Time
	Percentiles []float64 // besides 50, 95 and 99
	Window      int       // moving average and deviation over the last Window samples; 0 = none
}

// SummarySeries is one metric's statistics over the window, from raw samples.
type SummarySeries struct {
	DeviceID     uuid.UUID
	CheckID      uuid.UUID
	Object       string
	Name         string
	Unit         string
	Count        int
	Min, Max     float64
	Avg, StdDev  float64
	Percentiles  map[string]float64 // "p50", "p95", "p99" and the asked ones
	Last         float64
	LastAt       time.Time
	MovingAvg    float64 // over the last Window samples; NaN without a window
	MovingStdDev float64
	MovingCount  int
}

// MaxSummaryWindow is the largest moving window, in samples.
const MaxSummaryWindow = 10000

// Summary computes statistics over [From, To) per (check, object, name),
// merging series across layout changes, from raw samples.
func Summary(ctx context.Context, tx pgx.Tx, q SummaryQuery) ([]SummarySeries, error) {
	if q.To.IsZero() {
		q.To = time.Now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-time.Hour)
	}
	if !q.From.Before(q.To) {
		return nil, errs.Invalidf("from", "must be before to")
	}
	if q.Window < 0 || q.Window > MaxSummaryWindow {
		return nil, errs.Invalidf("window", "must be between 1 and %d samples", MaxSummaryWindow)
	}
	series, err := ListSeries(ctx, tx, q.Selector)
	if err != nil {
		return nil, err
	}
	if err := rawCovers(ctx, tx, series, q.From); err != nil {
		return nil, err
	}
	pcts := append([]float64{50, 95, 99}, q.Percentiles...)
	type key struct {
		check  uuid.UUID
		object string
		name   string
	}
	idx := map[key]int{}
	var out []SummarySeries
	var samples [][]rawSample
	budget := 0
	for _, s := range series {
		k := key{s.CheckID, s.Object, s.Name}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, SummarySeries{DeviceID: s.DeviceID, CheckID: s.CheckID, Object: s.Object, Name: s.Name, Unit: s.Unit})
			samples = append(samples, nil)
		}
		raw, err := readRaw(ctx, tx, s, q.From, q.To, &budget)
		if err != nil {
			return nil, err
		}
		samples[i] = append(samples[i], raw...)
	}
	for i := range out {
		ss := samples[i]
		slices.SortStableFunc(ss, func(a, b rawSample) int { return a.t.Compare(b.t) })
		o := &out[i]
		o.Count = len(ss)
		o.Percentiles = map[string]float64{}
		o.Min, o.Max, o.Avg, o.StdDev, o.Last = math.NaN(), math.NaN(), math.NaN(), math.NaN(), math.NaN()
		o.MovingAvg, o.MovingStdDev = math.NaN(), math.NaN()
		if len(ss) == 0 {
			continue
		}
		vals := make([]float64, len(ss))
		for j, s := range ss {
			vals[j] = s.v
		}
		o.Last, o.LastAt = vals[len(vals)-1], ss[len(ss)-1].t.UTC()
		if q.Window > 0 {
			tail := vals[max(len(vals)-q.Window, 0):]
			o.MovingAvg, o.MovingStdDev = MeanStdDev(tail)
			o.MovingCount = len(tail)
		}
		o.Avg, o.StdDev = MeanStdDev(vals)
		slices.Sort(vals)
		o.Min, o.Max = vals[0], vals[len(vals)-1]
		for _, p := range pcts {
			o.Percentiles[PercentileName(p)] = Percentile(vals, p)
		}
	}
	return out, nil
}

// PercentileName writes 95 as "p95" and 99.9 as "p99.9".
func PercentileName(p float64) string {
	return "p" + strconv.FormatFloat(p, 'f', -1, 64)
}
