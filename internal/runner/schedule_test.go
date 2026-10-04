package runner

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNextRunIsOnPhase(t *testing.T) {
	iv := time.Minute
	for range 100 {
		id := uuid.Must(uuid.NewV7())
		p := phase(id, iv)
		if p < 0 || p >= iv {
			t.Fatalf("phase %v outside [0, %v)", p, iv)
		}
		now := time.Now()
		n := nextRun(now, iv, p)
		if !n.After(now) || n.Sub(now) > iv {
			t.Fatalf("next %v not within one interval after %v", n, now)
		}
		if got := time.Duration(n.UnixMilli()%iv.Milliseconds()) * time.Millisecond; got != p {
			t.Fatalf("next run at offset %v, want phase %v", got, p)
		}
		if again := nextRun(n, iv, p); again.Sub(n) != iv {
			t.Fatalf("consecutive runs %v apart, want %v", again.Sub(n), iv)
		}
	}
}

// F04: the phase is stable for a check, so restarts do not move it.
func TestPhaseIsStable(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	first := phase(id, time.Minute)
	copied, _ := uuid.Parse(id.String()) // the same check loaded again
	if phase(copied, time.Minute) != first {
		t.Fatal("phase changed between loads")
	}
}

// Phases of many checks spread over the interval instead of clustering.
func TestPhasesSpread(t *testing.T) {
	buckets := make([]int, 10)
	for range 10000 {
		p := phase(uuid.Must(uuid.NewV7()), 10*time.Second)
		buckets[p/time.Second]++
	}
	for i, n := range buckets {
		if n < 800 || n > 1200 {
			t.Errorf("second %d has %d of 10000 checks; want about 1000", i, n)
		}
	}
}
