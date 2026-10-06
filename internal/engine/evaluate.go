// Package engine turns check results into state and state changes into
// incidents (F05). It is the only place where thresholds are evaluated and
// noise is controlled, so every plugin gets the same behavior.
package engine

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"time"

	"github.com/plusclouds/monitoring.server/internal/threshold"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Phases of a check's state machine.
const (
	PhaseOK      = "OK"
	PhasePending = "PENDING"
	PhaseProblem = "PROBLEM"
)

// Incident severities.
const (
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Flap detection over the last 20 results (F05): more than 30 % with a
// transition starts flapping, fewer than 20 % ends it.
const (
	flapWindow = 20
	flapStart  = 6 // > 30 % of 20
	flapStop   = 4 // < 20 % of 20
)

// RuleState is a threshold rule's progress between results.
type RuleState struct {
	Level        string    `json:"level,omitempty"` // applied level
	Pending      string    `json:"pending,omitempty"`
	PendingSince time.Time `json:"pending_since,omitzero"`
}

// WhoopsyState is the memory of the Whoopsy! band: the last values of its
// metric, how many results in a row left the band, and the band the last
// result was judged against.
type WhoopsyState struct {
	Metric string          `json:"metric"`
	Values []float64       `json:"values"`
	Hits   int             `json:"hits"`
	Last   float64         `json:"last"` // the last value judged, inside the band or not
	Level  string          `json:"level,omitempty"`
	Band   *threshold.Band `json:"band,omitempty"`
}

// State is a check's state machine, stored in check_state.
type State struct {
	Phase  string    `json:"phase"`
	Status string    `json:"status"` // last effective status: OK, WARNING, CRITICAL, UNKNOWN
	Since  time.Time `json:"since"`  // when Status last changed
	// AvailabilitySince is when the check last entered or left PROBLEM:
	// the device's up/down time when this is its host check.
	AvailabilitySince time.Time            `json:"availability_since,omitzero"`
	ConsecutiveBad    int                  `json:"consecutive_bad"`
	ConsecutiveGood   int                  `json:"consecutive_good"`
	Flapping          bool                 `json:"flapping"`
	History           uint32               `json:"history"` // bit 0 = latest result changed good/bad
	LastBad           bool                 `json:"last_bad"`
	Rules             map[string]RuleState `json:"rules,omitempty"`
	Whoopsy           *WhoopsyState        `json:"whoopsy,omitempty"`
	Severity          string               `json:"severity,omitempty"` // severity of the open incident
}

// Config is what evaluation needs from the check.
type Config struct {
	FailureCount      int
	RecoveryCount     int
	UnknownIsCritical bool
	Rules             []threshold.Rule
	Whoopsy           *threshold.Whoopsy
	Metrics           []string // the plugin's metric names, aligned with Input.Metrics
}

// Input is one result.
type Input struct {
	Status  plugin.Status
	Output  string
	Metrics []float64
	Time    time.Time
}

// Action is what evaluation asks the engine to do with incidents.
type Action int

const (
	ActionNone Action = iota
	ActionOpen
	ActionUpdate // severity or flapping changed while open
	ActionResolve
)

// Cause explains a bad result: the plugin, or a threshold rule.
type Cause struct {
	Rule   *threshold.Rule
	Level  string
	Value  float64
	Plugin bool
	// Whoopsy is set when the Whoopsy! band caused the level.
	Whoopsy *threshold.Whoopsy
	Band    *threshold.Band
}

// Outcome of evaluating one result.
type Outcome struct {
	State    State
	Action   Action
	Severity string // incident severity for open and update
	Cause    Cause
	Summary  string
}

// Evaluate applies one result to the state (F05). It is a pure function:
// the engine persists the returned state and acts on the outcome.
func Evaluate(cfg Config, prev State, in Input) Outcome {
	s := prev
	if s.Phase == "" {
		s.Phase, s.Since = PhaseOK, in.Time
	}
	if s.Rules == nil {
		s.Rules = map[string]RuleState{}
	}
	failures, recoveries := max(cfg.FailureCount, 1), max(cfg.RecoveryCount, 1)

	// UNKNOWN means the monitoring itself failed: its metrics say nothing,
	// and it neither opens nor resolves incidents unless the check says it
	// should (F05).
	if in.Status != plugin.OK && in.Status != plugin.Warning && in.Status != plugin.Critical {
		if s.Status != plugin.Unknown.String() {
			s.Status, s.Since = plugin.Unknown.String(), in.Time
		}
		if !cfg.UnknownIsCritical {
			return Outcome{State: s, Summary: summary(Cause{Plugin: true}, in)}
		}
		in.Status = plugin.Critical
	}

	// Thresholds, with their own memory of levels and pending durations.
	cause, immediate := Cause{}, false
	rules := map[string]RuleState{}
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		rs := s.Rules[r.ID]
		v := metric(cfg.Metrics, in.Metrics, r.Metric)
		target := r.Level(v, rs.Level)
		switch {
		case threshold.Rank(target) <= threshold.Rank(rs.Level) || r.For == 0:
			rs.Level, rs.Pending, rs.PendingSince = target, "", time.Time{}
		case rs.Pending != target:
			rs.Pending, rs.PendingSince = target, in.Time
		case in.Time.Sub(rs.PendingSince) >= r.For.D():
			rs.Level, rs.Pending, rs.PendingSince = target, "", time.Time{}
		}
		rules[r.ID] = rs
		if threshold.Rank(rs.Level) > threshold.Rank(cause.Level) {
			cause = Cause{Rule: r, Level: rs.Level, Value: v}
			immediate = r.For > 0 // the duration already filtered the noise
		}
	}
	s.Rules = rules

	// Whoopsy!: the metric against its own recent band.
	s.Whoopsy = nil
	if w := cfg.Whoopsy; w != nil {
		ws := prev.Whoopsy
		if ws == nil || ws.Metric != w.Metric {
			ws = &WhoopsyState{Metric: w.Metric}
		} else {
			c := *ws
			c.Values = slices.Clone(ws.Values)
			ws = &c
		}
		v := metric(cfg.Metrics, in.Metrics, w.Metric)
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			outside := false
			if len(ws.Values) >= w.Window {
				b := w.BandOf(ws.Values[len(ws.Values)-w.Window:])
				ws.Band = &b
				outside = w.Outside(b, v)
				if outside {
					ws.Hits++
				} else {
					ws.Hits = 0
				}
			}
			ws.Last = v
			// The band is frozen while values break it: an outlier never
			// enters the window, so a slowdown cannot widen the band and
			// resolve itself. Resetting Whoopsy! accepts a new normal.
			if !outside {
				ws.Values = append(ws.Values, v)
			}
			if extra := len(ws.Values) - w.Window; extra > 0 {
				ws.Values = ws.Values[extra:]
			}
			ws.Level = ""
			if ws.Hits >= w.Consecutive {
				ws.Level = w.Severity
			}
		}
		s.Whoopsy = ws
		if threshold.Rank(ws.Level) > threshold.Rank(cause.Level) {
			cause = Cause{Rule: &threshold.Rule{ID: threshold.WhoopsyRuleID, Name: threshold.WhoopsyName, Metric: w.Metric},
				Level: ws.Level, Value: v, Whoopsy: w, Band: ws.Band}
			immediate = true // consecutive results already filtered the noise
		}
	}

	// Effective status: the worse of the plugin's status and the thresholds.
	// The plugin is the cause when it is at least as bad as the thresholds.
	pluginLevel := map[plugin.Status]string{plugin.Warning: threshold.LevelWarning, plugin.Critical: threshold.LevelCritical}[in.Status]
	if pluginLevel != "" && threshold.Rank(pluginLevel) >= threshold.Rank(cause.Level) {
		cause, immediate = Cause{Plugin: true, Level: pluginLevel}, false
	}
	status, severity := plugin.OK, ""
	switch cause.Level {
	case threshold.LevelWarning:
		status, severity = plugin.Warning, SeverityWarning
	case threshold.LevelCritical:
		status, severity = plugin.Critical, SeverityCritical
	}
	if s.Status != status.String() {
		s.Status, s.Since = status.String(), in.Time
	}
	bad := status != plugin.OK

	// Flap detection: count good/bad changes over the last results.
	s.History <<= 1
	if bad != s.LastBad {
		s.History |= 1
	}
	s.History &= 1<<flapWindow - 1
	s.LastBad = bad
	changes := bits.OnesCount32(s.History)
	startFlap := !s.Flapping && changes > flapStart
	if startFlap {
		s.Flapping = true
	} else if s.Flapping && changes < flapStop {
		s.Flapping = false
	}

	out := Outcome{Severity: severity, Cause: cause, Summary: summary(cause, in)}
	if bad {
		s.ConsecutiveBad++
		s.ConsecutiveGood = 0
	} else {
		s.ConsecutiveGood++
		s.ConsecutiveBad = 0
	}

	switch s.Phase {
	case PhaseOK, PhasePending:
		switch {
		case bad && (s.ConsecutiveBad >= failures || immediate || s.Flapping):
			s.Phase, s.Severity = PhaseProblem, severity
			out.Action = ActionOpen
		case bad:
			s.Phase = PhasePending
		default:
			s.Phase = PhaseOK
		}
	case PhaseProblem:
		switch {
		case !bad && s.ConsecutiveGood >= recoveries && !s.Flapping:
			s.Phase, s.Severity = PhaseOK, ""
			out.Action = ActionResolve
		case bad && severity != s.Severity:
			s.Severity = severity
			out.Action = ActionUpdate
		case startFlap:
			out.Action, out.Severity = ActionUpdate, s.Severity
		}
	}
	if (prev.Phase == PhaseProblem) != (s.Phase == PhaseProblem) || s.AvailabilitySince.IsZero() {
		s.AvailabilitySince = in.Time
	}
	out.State = s
	return out
}

func metric(names []string, values []float64, name string) float64 {
	for i, n := range names {
		if n == name && i < len(values) {
			return values[i]
		}
	}
	return math.NaN()
}

func summary(c Cause, in Input) string {
	if c.Whoopsy != nil && c.Band != nil {
		w, b := c.Whoopsy, c.Band
		edge, side := b.Upper, "above"
		if c.Value < b.Lower {
			edge, side = b.Lower, "below"
		}
		return fmt.Sprintf("%s: %s = %s, %s %s (moving average %s ± %g standard deviations of %s over the last %d results, %d in a row)",
			threshold.WhoopsyName, w.Metric, num(c.Value), side, num(edge), num(b.Mean), w.Deviations, num(b.StdDev), w.Window, w.Consecutive)
	}
	if c.Rule != nil && (c.Level == threshold.LevelWarning || c.Level == threshold.LevelCritical) {
		cond := c.Rule.Critical
		if c.Level == threshold.LevelWarning {
			cond = c.Rule.Warning
		}
		name := c.Rule.Name
		if name == "" {
			name = c.Rule.Metric
		}
		limit := fmt.Sprintf("%s %g", cond.Op, cond.Value)
		if cond.ValueMax != nil {
			limit = fmt.Sprintf("%s %g..%g", cond.Op, cond.Value, *cond.ValueMax)
		}
		return fmt.Sprintf("%s: %s = %g (%s)", name, c.Rule.Metric, c.Value, limit)
	}
	out := in.Output
	if len(out) > 300 {
		out = out[:300]
	}
	return out
}

// num prints a value with at most three decimals.
func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}
