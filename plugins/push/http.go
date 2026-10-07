// Package push implements push.http (F08): devices POST JSON to the ingest
// listener and the check's config maps the body to metrics, a status and
// an output. Silence is the failure: the ingest role reports a check that
// has not heard from its device for missed_count intervals.
package push

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&HTTP{}) }

// Limits of one check.
const (
	MaxMetrics   = 32
	MaxBatch     = 1000 // messages in one array body
	maxOutputLen = 1024
)

// HTTPConfig of a push.http check. The check's interval is the expected
// time between messages.
type HTTPConfig struct {
	Metrics     map[string]string `json:"metrics,omitempty" jsonschema:"description=Metric name to a selector in the JSON body, e.g. {\"temperature\": \"$.sensors.temp\"}. Names are lowercase letters, digits and _. Numbers, numeric strings and booleans (1/0) are accepted"`
	Units       map[string]string `json:"units,omitempty" jsonschema:"description=Unit of a metric, e.g. {\"temperature\": \"celsius\"}"`
	Status      string            `json:"status,omitempty" jsonschema:"maxLength=256,description=Selector of a status field: ok, warning, critical, unknown (or 0..3, or true/false). Without it every message is OK and thresholds decide"`
	Output      string            `json:"output,omitempty" jsonschema:"maxLength=256,description=Selector of a text shown as the check output"`
	Timestamp   string            `json:"timestamp,omitempty" jsonschema:"maxLength=256,description=Selector of the message time: RFC 3339, or Unix seconds or milliseconds. Without it, or when a message lacks the field, the arrival time is used"`
	MissedCount int               `json:"missed_count,omitempty" jsonschema:"minimum=1,maximum=100,default=3,description=CRITICAL after this many intervals without a message"`
}

var httpDefaults = HTTPConfig{MissedCount: 3}

var metricName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// HTTP implements push.http.
type HTTP struct{}

func (*HTTP) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type: "push.http",
		Kind: plugin.KindIngester,
		Description: "Data a device pushes: POST JSON to the ingest URL with the check's token. The config maps the body " +
			"to metrics, a status and an output. CRITICAL when no message arrives for missed_count intervals.",
		ConfigSchema:    plugin.SchemaFor[HTTPConfig](),
		DefaultInterval: time.Minute,
		MinInterval:     10 * time.Second,
		BillingClass:    plugin.BillingPush,
	}
}

func (*HTTP) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, httpDefaults)
	if err != nil {
		return err
	}
	if len(cfg.Metrics) == 0 && cfg.Status == "" {
		return errors.New("set metrics, status or both: otherwise a message carries nothing")
	}
	if len(cfg.Metrics) > MaxMetrics {
		return fmt.Errorf("at most %d metrics", MaxMetrics)
	}
	for name, sel := range cfg.Metrics {
		if !metricName.MatchString(name) {
			return fmt.Errorf("metric name %q: lowercase letters, digits and _, starting with a letter, at most 64", name)
		}
		if _, err := parseSelector(sel); err != nil {
			return fmt.Errorf("metrics.%s: %w", name, err)
		}
	}
	for name, unit := range cfg.Units {
		if _, ok := cfg.Metrics[name]; !ok {
			return fmt.Errorf("units.%s: no such metric", name)
		}
		if len(unit) > 32 {
			return fmt.Errorf("units.%s: at most 32 characters", name)
		}
	}
	for field, sel := range map[string]string{"status": cfg.Status, "output": cfg.Output, "timestamp": cfg.Timestamp} {
		if sel == "" {
			continue
		}
		if _, err := parseSelector(sel); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	return nil
}

// MetricsFor names the metrics in alphabetical order.
func (*HTTP) MetricsFor(raw json.RawMessage) ([]plugin.MetricDef, error) {
	cfg, err := plugin.DecodeConfig(raw, httpDefaults)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Metrics))
	for n := range cfg.Metrics {
		names = append(names, n)
	}
	slices.Sort(names)
	defs := make([]plugin.MetricDef, len(names))
	for i, n := range names {
		defs[i] = plugin.MetricDef{Name: n, Unit: cfg.Units[n], Kind: "gauge", RetentionClass: plugin.RetentionStandard,
			Description: "pushed: " + cfg.Metrics[n]}
	}
	return defs, nil
}

// MissedCount is the number of silent intervals before CRITICAL.
func MissedCount(raw json.RawMessage) int {
	cfg, err := plugin.DecodeConfig(raw, httpDefaults)
	if err != nil || cfg.MissedCount < 1 {
		return httpDefaults.MissedCount
	}
	return cfg.MissedCount
}

// compiled is a config with parsed selectors.
type compiled struct {
	metrics                   []selector // aligned with MetricsFor
	status, output, timestamp selector
}

func compile(raw json.RawMessage) (compiled, error) {
	cfg, err := plugin.DecodeConfig(raw, httpDefaults)
	if err != nil {
		return compiled{}, err
	}
	var c compiled
	names := make([]string, 0, len(cfg.Metrics))
	for n := range cfg.Metrics {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		s, err := parseSelector(cfg.Metrics[n])
		if err != nil {
			return compiled{}, err
		}
		c.metrics = append(c.metrics, s)
	}
	for _, f := range []struct {
		src string
		dst *selector
	}{{cfg.Status, &c.status}, {cfg.Output, &c.output}, {cfg.Timestamp, &c.timestamp}} {
		if f.src == "" {
			continue
		}
		if *f.dst, err = parseSelector(f.src); err != nil {
			return compiled{}, err
		}
	}
	return c, nil
}

// Parse accepts one JSON object, or an array of them (a device that
// buffered while offline), oldest first.
func (*HTTP) Parse(raw json.RawMessage, body []byte) ([]plugin.Result, error) {
	c, err := compile(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid check config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("the body is not JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("the body has more than one JSON value")
	}
	var msgs []any
	switch d := doc.(type) {
	case []any:
		if len(d) == 0 {
			return nil, errors.New("the body is an empty array")
		}
		if len(d) > MaxBatch {
			return nil, fmt.Errorf("at most %d messages in one body", MaxBatch)
		}
		msgs = d
	case map[string]any:
		msgs = []any{d}
	default:
		return nil, errors.New("the body must be a JSON object or an array of objects")
	}
	out := make([]plugin.Result, 0, len(msgs))
	for i, m := range msgs {
		r, err := c.message(m)
		if err != nil {
			if len(msgs) > 1 {
				return nil, fmt.Errorf("message %d: %w", i, err)
			}
			return nil, err
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b plugin.Result) int { return a.Time.Compare(b.Time) })
	return out, nil
}

func (c compiled) message(m any) (plugin.Result, error) {
	if _, ok := m.(map[string]any); !ok {
		return plugin.Result{}, errors.New("a message must be a JSON object")
	}
	r := plugin.Result{Metrics: plugin.NaNs(len(c.metrics))}
	var found []string
	for i, sel := range c.metrics {
		if v, ok := sel.get(m); ok {
			if f, ok := number(v); ok {
				r.Metrics[i] = f
				found = append(found, strconv.FormatFloat(f, 'g', 6, 64))
			}
		}
	}
	r.Status = plugin.OK
	if c.status != nil {
		v, ok := c.status.get(m)
		if !ok {
			return plugin.Result{}, errors.New("the status field is missing")
		}
		st, ok := status(v)
		if !ok {
			return plugin.Result{}, fmt.Errorf("status %v: expected ok, warning, critical, unknown, 0..3 or true/false", v)
		}
		r.Status = st
	}
	if c.output != nil {
		if v, ok := c.output.get(m); ok {
			r.Output = text(v)
		}
	}
	if r.Output == "" {
		r.Output = "pushed: " + strings.Join(found, ", ")
		if len(found) == 0 {
			r.Output = "pushed"
		}
	}
	if len(r.Output) > maxOutputLen {
		r.Output = r.Output[:maxOutputLen]
	}
	// A message without its timestamp gets the arrival time.
	if v, ok := c.timestamp.get(m); c.timestamp != nil && ok {
		t, ok := timestamp(v)
		if !ok {
			return plugin.Result{}, fmt.Errorf("timestamp %v: expected RFC 3339 or Unix seconds or milliseconds", v)
		}
		r.Time = t
	}
	return r, nil
}

func number(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return 0, false
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(x), 64); err != nil {
			return 0, false
		}
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func status(v any) (plugin.Status, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return plugin.OK, true
		}
		return plugin.Critical, true
	case json.Number:
		n, err := x.Int64()
		if err != nil || n < 0 || n > 3 {
			return 0, false
		}
		return plugin.Status(n), true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "ok", "up", "0":
			return plugin.OK, true
		case "warning", "warn", "1":
			return plugin.Warning, true
		case "critical", "crit", "down", "error", "2":
			return plugin.Critical, true
		case "unknown", "3":
			return plugin.Unknown, true
		}
	}
	return 0, false
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func timestamp(v any) (time.Time, bool) {
	switch x := v.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.UTC(), true
		}
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return unix(f)
		}
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return unix(f)
		}
	}
	return time.Time{}, false
}

// unix reads seconds, or milliseconds when the number is too big for
// seconds (after the year 33658).
func unix(f float64) (time.Time, bool) {
	if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return time.Time{}, false
	}
	if f >= 1e12 {
		f /= 1000
	}
	if f >= 1e11 {
		return time.Time{}, false
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true
}
