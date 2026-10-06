// Package threshold holds metric threshold rules (F05). Rules are stored on
// checks and evaluated by the engine, never by plugins, so every plugin gets
// the same behavior.
package threshold

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Operators. between and outside use Value and ValueMax.
const (
	OpGT      = ">"
	OpGE      = ">="
	OpLT      = "<"
	OpLE      = "<="
	OpEQ      = "=="
	OpNE      = "!="
	OpBetween = "between"
	OpOutside = "outside"
)

var ops = []string{OpGT, OpGE, OpLT, OpLE, OpEQ, OpNE, OpBetween, OpOutside}

// Condition is one level of a rule.
type Condition struct {
	Op       string   `json:"op"`
	Value    float64  `json:"value"`
	ValueMax *float64 `json:"value_max,omitempty"`
}

// Rule is a threshold on one metric. It has a warning level, a critical
// level, or both.
type Rule struct {
	ID         string     `json:"id"`
	Name       string     `json:"name,omitempty"`
	Metric     string     `json:"metric"`
	Object     string     `json:"object,omitempty"` // collectors: "*" or an object key
	Warning    *Condition `json:"warning,omitempty"`
	Critical   *Condition `json:"critical,omitempty"`
	For        Duration   `json:"for,omitempty"`
	Hysteresis float64    `json:"hysteresis,omitempty"`
}

// AppliesTo reports whether the rule covers a collector object: a rule
// without an object (or "*") covers all of them, otherwise the object's key
// or name must match exactly.
func (r Rule) AppliesTo(key, name string) bool {
	return r.Object == "" || r.Object == "*" || r.Object == key || r.Object == name
}

// Duration is a time.Duration written as "5m" in JSON.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("%q is not a duration such as 30s or 5m", b)
	}
	*d = Duration(v)
	return nil
}

// Validate checks rules against the metric names the plugin reports and
// fills in missing rule IDs ("r1", "r2", ...). Unknown operators are rejected
// when the rule is saved (F05).
func Validate(rules []Rule, metrics []string) ([]Rule, error) {
	out := make([]Rule, len(rules))
	ids := map[string]bool{}
	for i, r := range rules {
		field := fmt.Sprintf("thresholds[%d]", i)
		if !slices.Contains(metrics, r.Metric) {
			return nil, errs.Invalidf(field+".metric", "%q is not a metric of this check; known: %v", r.Metric, metrics)
		}
		if r.Warning == nil && r.Critical == nil {
			return nil, errs.Invalidf(field, "needs a warning or a critical condition")
		}
		for level, c := range map[string]*Condition{"warning": r.Warning, "critical": r.Critical} {
			if c == nil {
				continue
			}
			if err := c.validate(field + "." + level); err != nil {
				return nil, err
			}
		}
		if r.For < 0 || r.For.D() > 24*time.Hour {
			return nil, errs.Invalidf(field+".for", "must be between 0 and 24h")
		}
		if r.Hysteresis < 0 || math.IsNaN(r.Hysteresis) || math.IsInf(r.Hysteresis, 0) {
			return nil, errs.Invalidf(field+".hysteresis", "must be a non-negative number")
		}
		if len(r.Name) > 200 {
			return nil, errs.Invalidf(field+".name", "must be at most 200 characters")
		}
		if r.ID == "" {
			for n := i + 1; ; n++ {
				if id := fmt.Sprintf("r%d", n); !ids[id] && !hasID(rules, id) {
					r.ID = id
					break
				}
			}
		}
		if len(r.ID) > 64 {
			return nil, errs.Invalidf(field+".id", "must be at most 64 characters")
		}
		if ids[r.ID] {
			return nil, errs.Invalidf(field+".id", "%q is used twice", r.ID)
		}
		ids[r.ID] = true
		out[i] = r
	}
	return out, nil
}

func hasID(rules []Rule, id string) bool {
	return slices.ContainsFunc(rules, func(r Rule) bool { return r.ID == id })
}

func (c *Condition) validate(field string) error {
	if !slices.Contains(ops, c.Op) {
		return errs.Invalidf(field+".op", "%q is not one of %v", c.Op, ops)
	}
	if math.IsNaN(c.Value) || math.IsInf(c.Value, 0) {
		return errs.Invalidf(field+".value", "must be a finite number")
	}
	ranged := c.Op == OpBetween || c.Op == OpOutside
	switch {
	case ranged && c.ValueMax == nil:
		return errs.Invalidf(field+".value_max", "is required for %s", c.Op)
	case ranged && !(*c.ValueMax > c.Value):
		return errs.Invalidf(field+".value_max", "must be greater than value")
	case !ranged && c.ValueMax != nil:
		return errs.Invalidf(field+".value_max", "is only used by between and outside")
	}
	return nil
}

// Levels of a rule, in increasing order of severity.
const (
	LevelNone     = ""
	LevelWarning  = "warning"
	LevelCritical = "critical"
)

// Rank orders levels: none < warning < critical.
func Rank(level string) int {
	switch level {
	case LevelWarning:
		return 1
	case LevelCritical:
		return 2
	}
	return 0
}

// Level is the level value reaches, given the level the rule is at now.
// A level that is active stays active until the value crosses back by the
// hysteresis margin (alert above 90, clear below 85), so values hovering at
// a threshold do not toggle. NaN keeps the current level: a metric that was
// not collected says nothing.
func (r Rule) Level(value float64, current string) string {
	if math.IsNaN(value) {
		return current
	}
	if r.Critical != nil && r.Critical.holds(value, Rank(current) >= Rank(LevelCritical), r.Hysteresis) {
		return LevelCritical
	}
	if r.Warning != nil && r.Warning.holds(value, Rank(current) >= Rank(LevelWarning), r.Hysteresis) {
		return LevelWarning
	}
	// A critical-only rule whose critical level clears through hysteresis
	// drops to none; a rule with both levels may drop to warning above.
	return LevelNone
}

// holds reports whether the condition is met. When active, the condition
// is relaxed by h: it must be crossed back by h to stop holding.
func (c *Condition) holds(v float64, active bool, h float64) bool {
	if !active {
		h = 0
	}
	switch c.Op {
	case OpGT:
		return v > c.Value-h
	case OpGE:
		return v >= c.Value-h
	case OpLT:
		return v < c.Value+h
	case OpLE:
		return v <= c.Value+h
	case OpEQ:
		return v == c.Value
	case OpNE:
		return v != c.Value
	case OpBetween:
		return v >= c.Value-h && v <= *c.ValueMax+h
	case OpOutside:
		return v < c.Value+h || v > *c.ValueMax-h
	}
	return false // unknown operators never fire; Validate rejects them on save
}
