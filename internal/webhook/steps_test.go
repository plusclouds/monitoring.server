package webhook

import (
	"testing"
	"time"
)

func TestScheduleNext(t *testing.T) {
	ist, _ := time.LoadLocation("Europe/Istanbul")
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, ist) }
	work := Schedule{Timezone: "Europe/Istanbul", Days: []string{"mon", "tue", "wed", "thu", "fri"}, From: "08:00", To: "18:00"}
	night := Schedule{Timezone: "Europe/Istanbul", From: "22:00", To: "06:00"}
	for name, c := range map[string]struct {
		s        Schedule
		now, out time.Time
	}{
		"inside":           {work, at(2026, 10, 7, 10, 0), at(2026, 10, 7, 10, 0)}, // Wednesday
		"before":           {work, at(2026, 10, 7, 7, 0), at(2026, 10, 7, 8, 0)},
		"after":            {work, at(2026, 10, 7, 19, 0), at(2026, 10, 8, 8, 0)},
		"weekend":          {work, at(2026, 10, 10, 10, 0), at(2026, 10, 12, 8, 0)}, // Saturday to Monday
		"night, evening":   {night, at(2026, 10, 7, 23, 0), at(2026, 10, 7, 23, 0)},
		"night, early":     {night, at(2026, 10, 7, 3, 0), at(2026, 10, 7, 3, 0)},
		"night, afternoon": {night, at(2026, 10, 7, 12, 0), at(2026, 10, 7, 22, 0)},
	} {
		if got := c.s.Next(c.now); !got.Equal(c.out) {
			t.Errorf("%s: Next(%v) = %v, want %v", name, c.now, got.In(ist), c.out)
		}
	}
}
