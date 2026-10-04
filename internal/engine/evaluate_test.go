package engine

import (
	"math"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/internal/threshold"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

type sim struct {
	t     *testing.T
	cfg   Config
	state State
	now   time.Time
	acts  []Action
}

func newSim(t *testing.T, cfg Config) *sim {
	return &sim{t: t, cfg: cfg, now: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

func (s *sim) step(status plugin.Status, metrics ...float64) Outcome {
	s.now = s.now.Add(time.Minute)
	out := Evaluate(s.cfg, s.state, Input{Status: status, Metrics: metrics, Time: s.now, Output: "out"})
	s.state = out.State
	if out.Action != ActionNone {
		s.acts = append(s.acts, out.Action)
	}
	return out
}

func (s *sim) count(a Action) int {
	n := 0
	for _, x := range s.acts {
		if x == a {
			n++
		}
	}
	return n
}

// F05: one failure is pending, not alerted; three in a row open one incident;
// recovery resolves it.
func TestFailureCount(t *testing.T) {
	s := newSim(t, Config{FailureCount: 3, RecoveryCount: 1})
	s.step(plugin.Critical)
	if s.state.Phase != PhasePending || len(s.acts) != 0 {
		t.Fatalf("after one failure: %+v %v", s.state, s.acts)
	}
	s.step(plugin.OK) // blip
	if s.state.Phase != PhaseOK || len(s.acts) != 0 {
		t.Fatalf("blip: %+v", s.state)
	}
	s.step(plugin.Critical)
	s.step(plugin.Critical)
	out := s.step(plugin.Critical)
	if out.Action != ActionOpen || out.Severity != SeverityCritical || s.state.Phase != PhaseProblem {
		t.Fatalf("third failure: %+v", out)
	}
	s.step(plugin.Critical)
	if out := s.step(plugin.OK); out.Action != ActionResolve {
		t.Fatalf("recovery: %+v", out)
	}
	if s.count(ActionOpen) != 1 || s.count(ActionResolve) != 1 {
		t.Errorf("actions: %v", s.acts)
	}
}

// F05: severity changes while open update the incident.
func TestSeverityChange(t *testing.T) {
	s := newSim(t, Config{FailureCount: 1})
	s.step(plugin.Warning)
	if out := s.step(plugin.Critical); out.Action != ActionUpdate || out.Severity != SeverityCritical {
		t.Fatalf("escalation: %+v", out)
	}
	if out := s.step(plugin.Critical); out.Action != ActionNone {
		t.Fatalf("no change: %+v", out)
	}
}

// F05 acceptance: a value oscillating between 88 and 92 with threshold 90 and
// hysteresis 5 produces one incident, not many.
func TestHysteresisOscillation(t *testing.T) {
	rule := threshold.Rule{ID: "r1", Metric: "cpu", Critical: &threshold.Condition{Op: ">", Value: 90}, Hysteresis: 5}
	s := newSim(t, Config{FailureCount: 1, Rules: []threshold.Rule{rule}, Metrics: []string{"cpu"}})
	for i := range 40 {
		v := 88.0
		if i%2 == 0 {
			v = 92
		}
		s.step(plugin.OK, v)
	}
	if s.count(ActionOpen) != 1 || s.count(ActionResolve) != 0 {
		t.Fatalf("actions: %v", s.acts)
	}
	if out := s.step(plugin.OK, 84); out.Action != ActionResolve {
		t.Fatalf("clearing below 85: %+v", out)
	}
}

// F05: `for` requires the condition to hold continuously before the level
// applies, then opens without waiting for failure_count.
func TestForDuration(t *testing.T) {
	rule := threshold.Rule{ID: "r1", Metric: "ms", Warning: &threshold.Condition{Op: ">", Value: 800},
		For: threshold.Duration(5 * time.Minute)}
	s := newSim(t, Config{FailureCount: 3, Rules: []threshold.Rule{rule}, Metrics: []string{"ms"}})
	for i := range 5 {
		if out := s.step(plugin.OK, 900); out.Action != ActionNone || s.state.Status != "OK" {
			t.Fatalf("minute %d: %+v", i+1, out)
		}
	}
	out := s.step(plugin.OK, 900)
	if out.Action != ActionOpen || out.Severity != SeverityWarning || out.Cause.Rule == nil || out.Cause.Rule.ID != "r1" {
		t.Fatalf("after 5 minutes: %+v", out)
	}
	if out.Summary != "ms: ms = 900 (> 800)" {
		t.Errorf("summary %q", out.Summary)
	}
	s.step(plugin.OK, 100)
	if s.state.Phase != PhaseOK {
		t.Errorf("recovery is immediate: %+v", s.state)
	}
}

// F05: UNKNOWN neither opens nor resolves, unless unknown_is_critical.
func TestUnknown(t *testing.T) {
	s := newSim(t, Config{FailureCount: 1})
	s.step(plugin.Critical)
	if out := s.step(plugin.Unknown); out.Action != ActionNone || s.state.Phase != PhaseProblem || s.state.Status != "UNKNOWN" {
		t.Fatalf("unknown while open: %+v", out)
	}
	s.step(plugin.OK)

	u := newSim(t, Config{FailureCount: 1, UnknownIsCritical: true})
	if out := u.step(plugin.Unknown); out.Action != ActionOpen || out.Severity != SeverityCritical {
		t.Fatalf("unknown_is_critical: %+v", out)
	}
}

// F05 acceptance: a check toggling every interval produces one flapping
// notification in 20 intervals, and stays open while flapping.
func TestFlapping(t *testing.T) {
	s := newSim(t, Config{FailureCount: 1, RecoveryCount: 1})
	for i := range 20 {
		st := plugin.OK
		if i%2 == 0 {
			st = plugin.Critical
		}
		s.step(st)
	}
	opens, resolves := s.count(ActionOpen), s.count(ActionResolve)
	if !s.state.Flapping {
		t.Fatal("not flapping after 20 toggles")
	}
	// The first few toggles open and resolve normally until flapping is
	// detected; after that the incident stays open.
	if opens > 4 || resolves > 4 || s.state.Phase != PhaseProblem {
		t.Fatalf("opens %d, resolves %d, phase %s: %v", opens, resolves, s.state.Phase, s.acts)
	}
	before := len(s.acts)
	for range 10 {
		s.step(plugin.Critical)
		s.step(plugin.OK)
	}
	if len(s.acts) != before {
		t.Errorf("events while flapping: %v", s.acts[before:])
	}
	for range 20 {
		s.step(plugin.OK)
	}
	if s.state.Flapping || s.state.Phase != PhaseOK || s.acts[len(s.acts)-1] != ActionResolve {
		t.Errorf("after settling: %+v %v", s.state, s.acts)
	}
}

func TestNaNKeepsLevel(t *testing.T) {
	rule := threshold.Rule{ID: "r1", Metric: "v", Critical: &threshold.Condition{Op: ">", Value: 1}}
	s := newSim(t, Config{FailureCount: 1, Rules: []threshold.Rule{rule}, Metrics: []string{"v"}})
	s.step(plugin.OK, 5)
	if out := s.step(plugin.OK, math.NaN()); out.Action != ActionNone || s.state.Phase != PhaseProblem {
		t.Fatalf("NaN: %+v", out)
	}
}
