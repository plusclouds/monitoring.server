package liveness

import (
	"strings"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	Reset()
	if err := Check(time.Minute); err != nil {
		t.Errorf("no loops: %v", err)
	}
	Beat("notifier")
	if err := Check(time.Minute); err != nil {
		t.Errorf("fresh: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := Check(time.Millisecond); err == nil || !strings.Contains(err.Error(), "notifier") {
		t.Errorf("stale: %v", err)
	}
	Reset()
}
