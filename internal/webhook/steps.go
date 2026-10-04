package webhook

import (
	"fmt"
	"slices"
	"time"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Step is one escalation step of a route (F06). The first step is the
// notification itself; later steps follow After the incident was notified.
type Step struct {
	After                time.Duration     `json:"-"`
	AfterSeconds         int               `json:"after_seconds"`
	Labels               map[string]string `json:"labels,omitempty"`
	OnlyIfUnacknowledged bool              `json:"only_if_unacknowledged,omitempty"`
	Schedule             *Schedule         `json:"schedule,omitempty"`
}

// Schedule restricts a step to time windows: business hours versus night.
// A step due outside its windows waits for the next one.
type Schedule struct {
	Timezone string   `json:"timezone"`       // IANA name; default UTC
	Days     []string `json:"days,omitempty"` // mon..sun; empty is every day
	From     string   `json:"from"`           // "08:00"
	To       string   `json:"to"`             // "18:00"; before From spans midnight
}

var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// MaxSteps is the most steps a route may have.
const MaxSteps = 10

func validateSteps(steps []Step) error {
	if len(steps) > MaxSteps {
		return errs.Invalidf("steps", "at most %d steps", MaxSteps)
	}
	for i, s := range steps {
		field := fmt.Sprintf("steps[%d]", i)
		switch {
		case i == 0 && (s.AfterSeconds != 0 || s.Schedule != nil || s.OnlyIfUnacknowledged):
			return errs.Invalidf(field, "the first step is the notification itself: after_seconds 0, no schedule or only_if_unacknowledged")
		case s.AfterSeconds < 0 || s.AfterSeconds > 7*24*3600:
			return errs.Invalidf(field+".after_seconds", "must be 0 to 604800")
		case i > 0 && s.AfterSeconds <= steps[i-1].AfterSeconds:
			return errs.Invalidf(field+".after_seconds", "must be later than the step before")
		case len(s.Labels) > 20:
			return errs.Invalidf(field+".labels", "at most 20 labels")
		}
		if s.Schedule != nil {
			if err := s.Schedule.validate(); err != nil {
				return errs.Invalidf(field+".schedule", "%s", err.Error())
			}
		}
	}
	return nil
}

func (s *Schedule) validate() error {
	if _, err := s.location(); err != nil {
		return err
	}
	for _, d := range s.Days {
		if !slices.Contains(weekdays, d) {
			return fmt.Errorf("day %q is not one of %v", d, weekdays)
		}
	}
	f, err := clock(s.From)
	if err != nil {
		return err
	}
	t, err := clock(s.To)
	if err != nil {
		return err
	}
	if f == t {
		return fmt.Errorf("from and to must differ")
	}
	return nil
}

func (s *Schedule) location() (*time.Location, error) {
	if s.Timezone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q", s.Timezone)
	}
	return loc, nil
}

func clock(v string) (time.Duration, error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a time like 08:00", v)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Next returns t when it is inside a window, else the start of the next
// window. A window spanning midnight belongs to the day it starts on.
func (s *Schedule) Next(t time.Time) time.Time {
	loc, err := s.location()
	if err != nil {
		return t
	}
	from, _ := clock(s.From)
	to, _ := clock(s.To)
	lt := t.In(loc)
	day := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	for i := -1; i <= 8; i++ {
		d := day.AddDate(0, 0, i)
		if len(s.Days) > 0 && !slices.Contains(s.Days, weekdays[d.Weekday()]) {
			continue
		}
		start, end := d.Add(from), d.Add(to)
		if to < from {
			end = d.AddDate(0, 0, 1).Add(to)
		}
		if !lt.Before(start) && lt.Before(end) {
			return t
		}
		if start.After(lt) {
			return start.UTC()
		}
	}
	return t
}

// stepsOf returns a route's steps; a route without steps has one, with the
// route's labels.
func (r Route) stepsOf() []Step {
	if len(r.Steps) == 0 {
		return []Step{{Labels: r.Labels}}
	}
	out := make([]Step, len(r.Steps))
	for i, s := range r.Steps {
		s.After = time.Duration(s.AfterSeconds) * time.Second
		labels := map[string]string{}
		for k, v := range r.Labels {
			labels[k] = v
		}
		for k, v := range s.Labels {
			labels[k] = v
		}
		s.Labels = labels
		out[i] = s
	}
	return out
}
