package selfmon

import (
	"math"
	"testing"
)

func TestQuantile(t *testing.T) {
	old := map[float64]float64{0.1: 10, 1: 10, 5: 10}
	// 100 new observations: 90 under 0.1 s, 9 between 0.1 and 1, 1 between 1 and 5.
	cur := map[float64]float64{0.1: 100, 1: 109, 5: 110}
	if q := quantile(0.5, cur, old, 100); q <= 0 || q > 0.1 {
		t.Errorf("p50 %v", q)
	}
	if q := quantile(0.99, cur, old, 100); q <= 0.1 || q > 1 {
		t.Errorf("p99 %v", q)
	}
	if q := quantile(1, cur, old, 100); q <= 1 || q > 5 {
		t.Errorf("p100 %v", q)
	}
	if q := quantile(0.99, cur, cur, 0); !math.IsNaN(q) {
		t.Errorf("no observations: %v", q)
	}
}
