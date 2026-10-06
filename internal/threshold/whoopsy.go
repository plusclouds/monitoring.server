package threshold

import (
	"math"
	"slices"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Whoopsy! is premium alerting on a check: an alert when a metric leaves
// its own recent band, the moving average of the last Window results plus
// or minus Deviations standard deviations, for Consecutive results in a
// row. With a moving average of 500 ms, a standard deviation of 100 ms and
// one deviation, anything above 600 ms counts.
type Whoopsy struct {
	Metric      string  `json:"metric"`
	Window      int     `json:"window"`      // results in the band, 3 to 1000
	Deviations  float64 `json:"deviations"`  // band half-width in standard deviations
	Consecutive int     `json:"consecutive"` // results outside the band in a row before alerting
	Direction   string  `json:"direction"`   // above, below or both
	MinDelta    float64 `json:"min_delta"`   // the band is at least this wide, in the metric's unit
	Severity    string  `json:"severity"`    // warning or critical
}

// Whoopsy! defaults.
const (
	WhoopsyWindow      = 7
	WhoopsyDeviations  = 1
	WhoopsyConsecutive = 3
	WhoopsyRuleID      = "whoopsy"
	WhoopsyName        = "Whoopsy!"
)

// Directions.
const (
	DirAbove = "above"
	DirBelow = "below"
	DirBoth  = "both"
)

// ValidateWhoopsy fills defaults and checks the settings against the
// plugin's metrics. defaultMetric is the plugin's choice, if any.
func ValidateWhoopsy(w Whoopsy, metrics []string, defaultMetric string) (Whoopsy, error) {
	if w.Metric == "" {
		w.Metric = defaultMetric
	}
	if w.Metric == "" {
		return w, errs.Invalidf("whoopsy.metric", "this plugin has no default; choose one of %v", metrics)
	}
	if !slices.Contains(metrics, w.Metric) {
		return w, errs.Invalidf("whoopsy.metric", "%q is not a metric of this check; known: %v", w.Metric, metrics)
	}
	if w.Window == 0 {
		w.Window = WhoopsyWindow
	}
	if w.Window < 3 || w.Window > 1000 {
		return w, errs.Invalidf("whoopsy.window", "must be between 3 and 1000 results")
	}
	if w.Deviations == 0 {
		w.Deviations = WhoopsyDeviations
	}
	if !(w.Deviations > 0 && w.Deviations <= 10) {
		return w, errs.Invalidf("whoopsy.deviations", "must be above 0 and at most 10")
	}
	if w.Consecutive == 0 {
		w.Consecutive = WhoopsyConsecutive
	}
	if w.Consecutive < 1 || w.Consecutive > 100 {
		return w, errs.Invalidf("whoopsy.consecutive", "must be between 1 and 100")
	}
	if w.Direction == "" {
		w.Direction = DirAbove
	}
	if !slices.Contains([]string{DirAbove, DirBelow, DirBoth}, w.Direction) {
		return w, errs.Invalidf("whoopsy.direction", "must be above, below or both")
	}
	if w.MinDelta < 0 || math.IsNaN(w.MinDelta) || math.IsInf(w.MinDelta, 0) {
		return w, errs.Invalidf("whoopsy.min_delta", "must be a non-negative number")
	}
	if w.Severity == "" {
		w.Severity = LevelWarning
	}
	if w.Severity != LevelWarning && w.Severity != LevelCritical {
		return w, errs.Invalidf("whoopsy.severity", "must be warning or critical")
	}
	return w, nil
}

// Band is the range a value may take: mean ± max(deviations × sd, min_delta).
type Band struct {
	Mean, StdDev float64
	Lower, Upper float64
}

// BandOf computes the band of the window's values (the results before the
// current one). The standard deviation is the sample one (n-1).
func (w Whoopsy) BandOf(values []float64) Band {
	var mean float64
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	var ss float64
	for _, v := range values {
		ss += (v - mean) * (v - mean)
	}
	sd := 0.0
	if len(values) > 1 {
		sd = math.Sqrt(ss / float64(len(values)-1))
	}
	half := max(w.Deviations*sd, w.MinDelta)
	return Band{Mean: mean, StdDev: sd, Lower: mean - half, Upper: mean + half}
}

// Outside reports whether v breaks the band in the watched direction. A
// value exactly on the band's edge is inside.
func (w Whoopsy) Outside(b Band, v float64) bool {
	switch w.Direction {
	case DirBelow:
		return v < b.Lower
	case DirBoth:
		return v < b.Lower || v > b.Upper
	}
	return v > b.Upper
}
