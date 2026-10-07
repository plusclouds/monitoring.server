// Package liveness records that the long-running loops of this process
// still turn (F11). The heartbeat is only sent while every loop that ever
// beat has beaten recently, so a process that is up but stuck goes silent.
package liveness

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	mu    sync.Mutex
	beats = map[string]time.Time{}
)

// Beat records that the named loop turned now.
func Beat(name string) {
	mu.Lock()
	beats[name] = time.Now()
	mu.Unlock()
}

// Check returns an error naming the loops that have not beaten within
// maxAge; nil when all are fresh (or none ever beat).
func Check(maxAge time.Duration) error {
	mu.Lock()
	defer mu.Unlock()
	var stale []string
	for name, t := range beats {
		if time.Since(t) > maxAge {
			stale = append(stale, fmt.Sprintf("%s (%s ago)", name, time.Since(t).Round(time.Second)))
		}
	}
	if len(stale) == 0 {
		return nil
	}
	slices.Sort(stale)
	return fmt.Errorf("loops not turning: %s", strings.Join(stale, ", "))
}

// Reset forgets every loop (tests).
func Reset() {
	mu.Lock()
	beats = map[string]time.Time{}
	mu.Unlock()
}
