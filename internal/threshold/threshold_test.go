package threshold

import (
	"math"
	"testing"
)

func f(v float64) *float64 { return &v }

func TestLevelWithHysteresis(t *testing.T) {
	r := Rule{Metric: "cpu", Warning: &Condition{Op: OpGT, Value: 80}, Critical: &Condition{Op: OpGT, Value: 90}, Hysteresis: 5}
	steps := []struct {
		v    float64
		want string
	}{
		{70, LevelNone}, {85, LevelWarning}, {91, LevelCritical},
		{88, LevelCritical}, // within 5 of 90: stays critical
		{92, LevelCritical}, {86, LevelCritical},
		{84, LevelWarning}, // crossed back below 85
		{77, LevelWarning}, // within 5 of 80
		{74, LevelNone},
		{math.NaN(), LevelNone},
	}
	cur := LevelNone
	for i, s := range steps {
		cur = r.Level(s.v, cur)
		if cur != s.want {
			t.Fatalf("step %d (%v): level %q, want %q", i, s.v, cur, s.want)
		}
	}
}

func TestOperators(t *testing.T) {
	cases := []struct {
		c    Condition
		v    float64
		want bool
	}{
		{Condition{Op: OpLT, Value: 10}, 9, true},
		{Condition{Op: OpLE, Value: 10}, 10, true},
		{Condition{Op: OpGE, Value: 10}, 10, true},
		{Condition{Op: OpEQ, Value: 3}, 3, true},
		{Condition{Op: OpNE, Value: 3}, 3, false},
		{Condition{Op: OpBetween, Value: 10, ValueMax: f(50)}, 30, true},
		{Condition{Op: OpBetween, Value: 10, ValueMax: f(50)}, 51, false},
		{Condition{Op: OpOutside, Value: 10, ValueMax: f(50)}, 5, true},
		{Condition{Op: OpOutside, Value: 10, ValueMax: f(50)}, 30, false},
		{Condition{Op: "~"}, 1, false},
	}
	for _, c := range cases {
		if got := c.c.holds(c.v, false, 0); got != c.want {
			t.Errorf("%s %v on %v = %v, want %v", c.c.Op, c.c.Value, c.v, got, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	rules, err := Validate([]Rule{
		{Metric: "a", Critical: &Condition{Op: OpGT, Value: 1}},
		{Metric: "a", ID: "r1", Warning: &Condition{Op: OpGT, Value: 0}},
	}, []string{"a"})
	if err != nil || rules[0].ID != "r2" || rules[1].ID != "r1" {
		t.Fatalf("ids: %+v %v", rules, err)
	}
	bad := [][]Rule{
		{{Metric: "b", Critical: &Condition{Op: OpGT}}},
		{{Metric: "a"}},
		{{Metric: "a", Critical: &Condition{Op: "~"}}},
		{{Metric: "a", Critical: &Condition{Op: OpBetween, Value: 5, ValueMax: f(1)}}},
		{{Metric: "a", Critical: &Condition{Op: OpGT, ValueMax: f(1)}}},
		{{Metric: "a", ID: "x", Critical: &Condition{Op: OpGT}}, {Metric: "a", ID: "x", Critical: &Condition{Op: OpGT}}},
	}
	for i, rs := range bad {
		if _, err := Validate(rs, []string{"a"}); err == nil {
			t.Errorf("case %d: want an error", i)
		}
	}
}
