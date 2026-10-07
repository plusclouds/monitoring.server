package push

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() {
	plugin.Register(&MQTT{})
	plugin.Register(&Connection{})
}

// MQTT payload profiles (F08, F12).
const (
	// ProfileFixLean is the FixLean ESP32 sensors' flat JSON: a status
	// payload (firmware, RSSI, memory) or telemetry, never both.
	ProfileFixLean = "fixlean-esp"
	// ProfileJSON is any JSON object: every numeric field becomes a metric
	// (nested objects joined with _), or the config's selectors pick them.
	ProfileJSON = "json"
)

// Profiles lists the built-in profiles with a description.
var Profiles = []struct{ Name, Description string }{
	{ProfileFixLean, "FixLean ESP32 sensors: topic <vendor>/<MAC>/..., flat JSON. Status payloads (Product, Version, RSSI, ...) " +
		"update the device inventory and the rssi_dbm, runtime_ms and memory metrics; telemetry payloads store every numeric " +
		"field except unixtimestamp, sampling_rate, sample_size and measurement_buffer_size. Time from unixtimestamp (seconds)."},
	{ProfileJSON, "Any JSON object on <prefix>/<device_key>/...: every numeric field (nested objects joined with _) becomes a " +
		"metric, or the check's metrics selectors pick them as for push.http."},
}

// Status metrics of the FixLean profile, always first in its layout.
var fixleanStatusMetrics = []plugin.MetricDef{
	{Name: "rssi_dbm", Unit: "dBm", Kind: "gauge", RetentionClass: plugin.RetentionStandard, Description: "Wi-Fi signal strength"},
	{Name: "runtime_ms", Unit: "ms", Kind: "gauge", RetentionClass: plugin.RetentionStandard, Description: "Time since the last reset"},
	{Name: "memory_free_bytes", Unit: "bytes", Kind: "gauge", RetentionClass: plugin.RetentionStandard, Description: "Free heap"},
	{Name: "memory_min_free_bytes", Unit: "bytes", Kind: "gauge", RetentionClass: plugin.RetentionStandard, Description: "Lowest free heap since reset"},
}

// FixLean payload keys, as the current collector reads them.
var (
	fixleanStatusKeys = []string{"Product", "Version", "RSSI", "Local IP", "Network MAC", "Last Reset Reason", "Runtime MS",
		"Memory Info", "ESP-IDF Version", "Compile Date", "Compile Time", "Current Running Application", "status"}
	fixleanMetadata = []string{"unixtimestamp", "sampling_rate", "sample_size", "measurement_buffer_size"}
	// Status fields kept in the device's inventory.
	fixleanInventory = map[string]string{"Product": "product", "Version": "firmware", "ESP-IDF Version": "esp_idf",
		"Current Running Application": "application", "Last Reset Reason": "last_reset_reason", "Local IP": "local_ip",
		"Network MAC": "network_mac"}
)

// MQTTConfig of a push.mqtt check. Auto-registration creates it; Fields
// grows as the device sends new keys.
type MQTTConfig struct {
	Profile     string            `json:"profile,omitempty" jsonschema:"enum=fixlean-esp,enum=json,default=json,description=Payload profile; see GET /v1/ingest/profiles"`
	Fields      []string          `json:"fields,omitempty" jsonschema:"maxItems=500,description=Discovered metric names (the engine adds new keys up to ingest.mqtt.max_discovered_fields)"`
	Metrics     map[string]string `json:"metrics,omitempty" jsonschema:"description=json profile only: metric name to selector, as for push.http. Set, it replaces field discovery"`
	Units       map[string]string `json:"units,omitempty" jsonschema:"description=Unit of a metric"`
	Status      string            `json:"status,omitempty" jsonschema:"maxLength=256,description=json profile: selector of a status field"`
	Output      string            `json:"output,omitempty" jsonschema:"maxLength=256,description=json profile: selector of an output text"`
	Timestamp   string            `json:"timestamp,omitempty" jsonschema:"maxLength=256,description=json profile: selector of the message time (RFC 3339 or Unix seconds or milliseconds)"`
	MissedCount int               `json:"missed_count,omitempty" jsonschema:"minimum=1,maximum=100,default=3,description=CRITICAL after this many intervals without a message"`
}

var mqttDefaults = MQTTConfig{Profile: ProfileJSON, MissedCount: 3}

// MQTT implements push.mqtt: data a device publishes to the embedded broker.
type MQTT struct{}

func (*MQTT) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type: "push.mqtt",
		Kind: plugin.KindIngester,
		Description: "Data a device publishes to the embedded MQTT broker, mapped by a profile (fixlean-esp or json). " +
			"Usually created by auto-registration. CRITICAL when no message arrives for missed_count intervals.",
		ConfigSchema:    plugin.SchemaFor[MQTTConfig](),
		DefaultInterval: 5 * time.Minute,
		MinInterval:     10 * time.Second,
		BillingClass:    plugin.BillingPush,
	}
}

func (*MQTT) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, mqttDefaults)
	if err != nil {
		return err
	}
	if cfg.Profile == ProfileFixLean && (len(cfg.Metrics) > 0 || cfg.Status != "" || cfg.Output != "" || cfg.Timestamp != "") {
		return errors.New("metrics, status, output and timestamp are for the json profile")
	}
	seen := map[string]bool{}
	for _, f := range cfg.Fields {
		if !metricName.MatchString(f) || seen[f] {
			return fmt.Errorf("fields: %q is not a unique metric name (lowercase letters, digits and _)", f)
		}
		seen[f] = true
	}
	if len(cfg.Metrics) > 0 {
		h := HTTPConfig{Metrics: cfg.Metrics, Units: cfg.Units, Status: cfg.Status, Output: cfg.Output, Timestamp: cfg.Timestamp}
		b, _ := json.Marshal(h)
		return (&HTTP{}).Validate(b)
	}
	for name := range cfg.Units {
		if !seen[name] && !slices.ContainsFunc(fixleanStatusMetrics, func(d plugin.MetricDef) bool { return d.Name == name }) {
			return fmt.Errorf("units.%s: no such metric", name)
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

// MetricsFor: the FixLean status metrics, then the discovered fields in
// name order; or the json profile's selectors.
func (*MQTT) MetricsFor(raw json.RawMessage) ([]plugin.MetricDef, error) {
	cfg, err := plugin.DecodeConfig(raw, mqttDefaults)
	if err != nil {
		return nil, err
	}
	if len(cfg.Metrics) > 0 {
		b, _ := json.Marshal(HTTPConfig{Metrics: cfg.Metrics, Units: cfg.Units})
		return (&HTTP{}).MetricsFor(b)
	}
	var defs []plugin.MetricDef
	if cfg.Profile == ProfileFixLean {
		for _, d := range fixleanStatusMetrics {
			if u, ok := cfg.Units[d.Name]; ok {
				d.Unit = u
			}
			defs = append(defs, d)
		}
	}
	fields := slices.Clone(cfg.Fields)
	slices.Sort(fields)
	for _, f := range fields {
		if slices.ContainsFunc(defs, func(d plugin.MetricDef) bool { return d.Name == f }) {
			continue
		}
		defs = append(defs, plugin.MetricDef{Name: f, Unit: cfg.Units[f], Kind: "gauge", RetentionClass: plugin.RetentionStandard,
			Description: "discovered"})
	}
	return defs, nil
}

// Message is one decoded MQTT payload.
type Message struct {
	Status  bool               // a FixLean status payload
	Info    map[string]string  // inventory fields of a status payload
	Values  map[string]float64 // metric name to value
	Time    time.Time          // zero: arrival time
	Skipped int                // fields that are not numbers
}

// Discoverable reports whether the config learns its metrics from the
// payloads (no explicit selectors).
func Discoverable(raw json.RawMessage) bool {
	cfg, err := plugin.DecodeConfig(raw, mqttDefaults)
	return err == nil && len(cfg.Metrics) == 0
}

// Decode reads a payload of a profile.
func Decode(profile string, body []byte) (Message, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return Message{}, fmt.Errorf("the payload is not a JSON object: %w", err)
	}
	if dec.More() {
		return Message{}, errors.New("the payload has more than one JSON value")
	}
	m := Message{Values: map[string]float64{}}
	if profile != ProfileFixLean {
		flatten("", doc, 0, &m)
		return m, nil
	}
	if v, ok := doc["unixtimestamp"]; ok {
		if f, ok := number(v); ok {
			if t, ok := unix(f); ok {
				m.Time = t
			}
		}
	}
	if slices.ContainsFunc(fixleanStatusKeys, func(k string) bool { _, ok := doc[k]; return ok }) {
		m.Status = true
		m.Info = map[string]string{}
		for k, name := range fixleanInventory {
			if s := text(doc[k]); s != "" {
				m.Info[name] = truncate(s, 200)
			}
		}
		if d, t := text(doc["Compile Date"]), text(doc["Compile Time"]); d != "" {
			m.Info["compiled"] = truncate(strings.TrimSpace(d+" "+t), 200)
		}
		if f, ok := number(doc["RSSI"]); ok {
			m.Values["rssi_dbm"] = f
		}
		if f, ok := number(doc["Runtime MS"]); ok {
			m.Values["runtime_ms"] = f
		}
		memory(doc["Memory Info"], &m)
		return m, nil
	}
	for k, v := range doc {
		if slices.Contains(fixleanMetadata, k) {
			continue
		}
		name, ok := MetricName(k)
		if !ok {
			m.Skipped++
			continue
		}
		if f, ok := v.(json.Number); ok {
			if x, err := f.Float64(); err == nil && !math.IsNaN(x) && !math.IsInf(x, 0) {
				m.Values[name] = x
				continue
			}
		}
		m.Skipped++
	}
	return m, nil
}

// memory reads "Memory Info": a number (free bytes) or an object whose
// keys mention free and min.
func memory(v any, m *Message) {
	if f, ok := number(v); ok {
		m.Values["memory_free_bytes"] = f
		return
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k, x := range obj {
		f, ok := number(x)
		if !ok {
			continue
		}
		lk := strings.ToLower(k)
		switch {
		case strings.Contains(lk, "min"):
			m.Values["memory_min_free_bytes"] = f
		case strings.Contains(lk, "free"):
			m.Values["memory_free_bytes"] = f
		}
	}
}

// flatten collects numeric fields, nested objects joined with _, at most
// 3 levels deep.
func flatten(prefix string, v any, depth int, m *Message) {
	switch x := v.(type) {
	case map[string]any:
		if depth >= 3 {
			m.Skipped++
			return
		}
		for k, sub := range x {
			key := k
			if prefix != "" {
				key = prefix + "_" + k
			}
			flatten(key, sub, depth+1, m)
		}
	case json.Number, bool:
		name, ok := MetricName(prefix)
		f, isNum := number(x)
		if !ok || !isNum {
			m.Skipped++
			return
		}
		m.Values[name] = f
	default:
		m.Skipped++
	}
}

// MetricName turns a payload key into a metric name: lowercase, other
// characters as _, a leading digit prefixed with m_.
func MetricName(key string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	if s == "" {
		return "", false
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "m_" + s
	}
	if len(s) > 64 {
		s = strings.TrimRight(s[:64], "_")
	}
	return s, metricName.MatchString(s)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Parse turns one payload into a result aligned with MetricsFor(cfg).
// Keys that are not (yet) in the layout are left out.
func (p *MQTT) Parse(raw json.RawMessage, body []byte) ([]plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(raw, mqttDefaults)
	if err != nil {
		return nil, fmt.Errorf("invalid check config: %w", err)
	}
	if len(cfg.Metrics) > 0 || cfg.Status != "" {
		c, err := compileMapping(cfg.Metrics, cfg.Status, cfg.Output, cfg.Timestamp)
		if err != nil {
			return nil, err
		}
		return c.parse(body)
	}
	msg, err := Decode(cfg.Profile, body)
	if err != nil {
		return nil, err
	}
	defs, err := p.MetricsFor(raw)
	if err != nil {
		return nil, err
	}
	r := plugin.Result{Status: plugin.OK, Metrics: plugin.NaNs(len(defs)), Time: msg.Time}
	n := 0
	for i, d := range defs {
		if v, ok := msg.Values[d.Name]; ok {
			r.Metrics[i] = v
			n++
		}
	}
	if cfg.Timestamp != "" {
		if sel, err := parseSelector(cfg.Timestamp); err == nil {
			var doc any
			d := json.NewDecoder(bytes.NewReader(body))
			d.UseNumber()
			if d.Decode(&doc) == nil {
				if v, ok := sel.get(doc); ok {
					if t, ok := timestamp(v); ok {
						r.Time = t
					}
				}
			}
		}
	}
	switch {
	case msg.Status:
		parts := []string{"status"}
		for _, k := range []string{"product", "firmware"} {
			if v := msg.Info[k]; v != "" {
				parts = append(parts, k+" "+v)
			}
		}
		if v, ok := msg.Values["rssi_dbm"]; ok {
			parts = append(parts, "RSSI "+strconv.FormatFloat(v, 'f', -1, 64)+" dBm")
		}
		r.Output = strings.Join(parts, ", ")
	case n == 0:
		r.Output = "message without known numeric fields"
	default:
		r.Output = fmt.Sprintf("%d values", n)
	}
	if msg.Skipped > 0 {
		r.Output += fmt.Sprintf(" (%d non-numeric fields ignored)", msg.Skipped)
	}
	return []plugin.Result{r}, nil
}

// MQTTMissedCount is the number of silent intervals before CRITICAL.
func MQTTMissedCount(raw json.RawMessage) int {
	cfg, err := plugin.DecodeConfig(raw, mqttDefaults)
	if err != nil || cfg.MissedCount < 1 {
		return mqttDefaults.MissedCount
	}
	return cfg.MissedCount
}

// Connection implements mqtt.connection: the broker reports a device's
// MQTT session. Connected and sending is OK; a disconnect or a keepalive
// timeout is CRITICAL. It is the device's host check, so an offline sensor
// suppresses its threshold incidents.
type Connection struct{}

func (*Connection) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type: "mqtt.connection",
		Kind: plugin.KindIngester,
		Description: "A device's connection to the embedded MQTT broker, from broker events: CRITICAL on disconnect or " +
			"keepalive timeout, OK again when data flows. Created by auto-registration as the device's host check.",
		ConfigSchema:    plugin.SchemaFor[struct{}](),
		DefaultInterval: 5 * time.Minute,
		MinInterval:     10 * time.Second,
		BillingClass:    plugin.BillingFree,
	}
}

func (*Connection) Validate(json.RawMessage) error { return nil }

func (*Connection) MetricsFor(json.RawMessage) ([]plugin.MetricDef, error) { return nil, nil }

func (*Connection) Parse(json.RawMessage, []byte) ([]plugin.Result, error) {
	return nil, errors.New("mqtt.connection is fed by the broker, not by pushed messages")
}
