package webhook

import (
	"testing"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/incident"
)

// v0.3.1: a route can take the incidents of chosen checks or devices only.
func TestMatchByCheckAndDevice(t *testing.T) {
	dev, chk, other := uuid.New(), uuid.New(), uuid.New()
	e := &incident.Envelope{Type: incident.EventOpened, Data: incident.Data{
		Device: &incident.DeviceRef{ID: dev, Type: "web"}, Check: &incident.CheckRef{ID: chk}}}
	for name, c := range map[string]struct {
		m    Match
		want bool
	}{
		"empty":          {Match{}, true},
		"check":          {Match{CheckIDs: []uuid.UUID{other, chk}}, true},
		"other check":    {Match{CheckIDs: []uuid.UUID{other}}, false},
		"device":         {Match{DeviceIDs: []uuid.UUID{dev}}, true},
		"other device":   {Match{DeviceIDs: []uuid.UUID{other}}, false},
		"both, all must": {Match{DeviceIDs: []uuid.UUID{dev}, CheckIDs: []uuid.UUID{other}}, false},
	} {
		if got := (Route{Match: c.m}).Matches(e, "critical"); got != c.want {
			t.Errorf("%s: matches = %v, want %v", name, got, c.want)
		}
	}
}
