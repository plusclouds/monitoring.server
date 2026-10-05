package snmp

import (
	"encoding/json"
	"math"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Counter handling (F10, design section 9): rate = delta / elapsed seconds
// between collection times. A negative delta is a reset when the uptime went
// backwards; otherwise a 32-bit counter is assumed to have wrapped once if
// the resulting rate is plausible. Rates above the maximum plausible rate
// × 1.1 are glitches.

// sample is one counter reading kept in the plugin state between runs.
type sample struct {
	Value  float64   `json:"v"`
	Bits   int       `json:"b"` // 32 or 64
	At     time.Time `json:"t"`
	Uptime float64   `json:"u,omitempty"` // seconds; 0 = unknown
}

// rate computes the per-second rate from prev to cur. maxRate, when above 0,
// is the highest plausible rate (for interfaces, the interface speed). It
// returns false when there is no usable rate this time.
func rate(prev, cur sample, maxRate float64) (float64, bool) {
	elapsed := cur.At.Sub(prev.At).Seconds()
	if elapsed <= 0 || prev.Bits != cur.Bits {
		return 0, false
	}
	if prev.Uptime > 0 && cur.Uptime > 0 && restarted(prev.Uptime, cur.Uptime, elapsed) {
		return 0, false
	}
	delta := cur.Value - prev.Value
	if delta < 0 {
		if cur.Bits != 32 {
			return 0, false // a 64-bit counter does not wrap in practice: a reset
		}
		delta += math.Exp2(32)
	}
	r := delta / elapsed
	if maxRate > 0 && r > maxRate*1.1 {
		return 0, false
	}
	return r, true
}

// timeTicksWrap is when a TimeTicks uptime wraps: 2^32 hundredths of a
// second, about 497 days.
var timeTicksWrap = math.Exp2(32) / 100

// restarted reports whether an uptime going from prev to cur over elapsed
// seconds means the device or agent restarted, as opposed to the TimeTicks
// value wrapping after 497 days.
func restarted(prev, cur, elapsed float64) bool {
	if cur >= prev {
		return false
	}
	expected := prev + elapsed
	if expected >= timeTicksWrap {
		tolerance := max(30, elapsed/10)
		if math.Abs(cur-(expected-timeTicksWrap)) <= tolerance {
			return false
		}
	}
	return true
}

// loadSample and saveSample keep a sample under a state key.
func loadSample(st plugin.StateStore, key string) (sample, bool) {
	if st == nil {
		return sample{}, false
	}
	b, ok := st.Get(key)
	if !ok {
		return sample{}, false
	}
	var s sample
	if json.Unmarshal(b, &s) != nil {
		return sample{}, false
	}
	return s, true
}

func saveSample(st plugin.StateStore, key string, s sample) {
	if st == nil {
		return
	}
	if b, err := json.Marshal(s); err == nil {
		_ = st.Set(key, b)
	}
}
