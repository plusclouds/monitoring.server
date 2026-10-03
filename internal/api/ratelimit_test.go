package api

import (
	"testing"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// A plan change applies at once: raising a drained key's limit must not
// keep refusing requests until the old bucket refills.
func TestRaisedLimitAppliesImmediately(t *testing.T) {
	s := &Server{keyLimits: map[uuid.UUID]*rate.Limiter{}}
	id := uuid.New()
	if !s.keyLimiter(id, 6).Allow() {
		t.Fatal("first request should pass")
	}
	if s.keyLimiter(id, 6).Allow() {
		t.Fatal("burst of 1 should be used up")
	}
	if !s.keyLimiter(id, 1200).Allow() {
		t.Error("request right after raising the limit was refused")
	}
}
