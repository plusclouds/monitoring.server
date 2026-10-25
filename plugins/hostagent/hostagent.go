// Package hostagent implements host.agent: the report the monitoring agent
// (cmd/agent) POSTs to the ingest listener with a token minted for monitoring
// alone (push token, F08, F15). The agent runs inside a Docker image or beside
// it and reports objects: the host or container it runs in, the containers it
// watches, local probes (process, port, HTTP) and services (postgres, redis,
// nginx ...). Each object has its own metrics, thresholds, state and incidents.
//
// The agent reports facts and a status; the server decides. Silence is
// CRITICAL through the last-seen rule (missed_count intervals).
//
// The metric layout is one catalog shared by every object kind; an object
// fills the slots that mean something for it, and the rest stay NaN. An agent
// metric the catalog does not know is dropped and counted in the output, so a
// newer agent does not break an older server.
package hostagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Agent{}) }

// Limits against a broken or hostile agent.
const (
	maxObjects   = 256
	maxLabels    = 16
	maxKeyBytes  = 200
	maxNameBytes = 200
	maxOutput    = 1 << 10
)

// catalog is the metric layout: name, unit, description. Order is the slot
// number; append only, never reorder (stored series are keyed by slot).
var catalog = []struct{ name, unit, desc string }{
	// host and container-wide (kind host)
	{"cpu_usage_pct", "percent", "host: CPU busy (container: of its CPU limit)"},
	{"load1", "", "host: 1-minute load average"},
	{"load5", "", "host: 5-minute load average"},
	{"load15", "", "host: 15-minute load average"},
	{"mem_usage_pct", "percent", "host: memory used (container: of its limit)"},
	{"mem_used_bytes", "bytes", "host, container, service: memory used"},
	{"mem_total_bytes", "bytes", "host: memory available (container: its limit)"},
	{"swap_usage_pct", "percent", "host: swap used"},
	{"disk_usage_pct", "percent", "host: fullest watched filesystem"},
	{"disk_used_bytes", "bytes", "host: fullest watched filesystem, used"},
	{"disk_total_bytes", "bytes", "host: fullest watched filesystem, size"},
	{"net_rx_bytes_per_sec", "bytes", "host, container: network received"},
	{"net_tx_bytes_per_sec", "bytes", "host, container: network sent"},
	{"uptime_seconds", "s", "host, container, service: time since start"},
	{"oom_kills", "count", "host, container: OOM kills since start"},
	// containers (kind container)
	{"container_running", "", "container: 1 running, 0 not"},
	{"container_healthy", "", "container: 1 health check passes (or none defined), 0 fails"},
	{"container_restarts", "count", "container: restarts since created"},
	{"container_exit_code", "", "container: exit code of the last stop"},
	// probes (kind process, tcp, http) and services (kind service)
	{"up", "", "probe, service: 1 answers, 0 does not"},
	{"response_ms", "ms", "probe, service: time to answer"},
	{"http_status", "", "http probe: status code"},
	{"process_count", "count", "process probe: matching processes"},
	{"connections", "count", "service: client connections"},
	{"connections_max", "count", "service: connection limit"},
	{"connections_usage_pct", "percent", "service: connections of the limit"},
	{"ops_per_sec", "", "service: operations (queries, commands, requests) per second"},
	{"errors_per_sec", "", "service: errors per second"},
	{"cache_hit_pct", "percent", "service: cache or buffer hit ratio"},
	{"data_size_bytes", "bytes", "service: data held (database, keyspace, index)"},
	{"replication_lag_seconds", "s", "service: replica behind its primary"},
	{"slow_queries", "count", "service: slow queries since start"},
	{"locks_waiting", "count", "service: sessions waiting on a lock"},
	// service specific
	{"redis_evicted_keys", "count", "redis: keys evicted since start"},
	{"redis_keys", "count", "redis: keys held"},
	{"redis_blocked_clients", "count", "redis: clients blocked on a command"},
	{"pg_deadlocks", "count", "postgres: deadlocks since stats reset"},
	{"pg_longest_query_seconds", "s", "postgres: longest running query"},
	{"pg_xid_age", "count", "postgres: oldest transaction ID age (wraparound risk)"},
	{"nginx_active", "count", "nginx: active connections"},
	{"nginx_waiting", "count", "nginx: idle keep-alive connections"},
	{"phpfpm_active", "count", "php-fpm: active workers"},
	{"phpfpm_idle", "count", "php-fpm: idle workers"},
	{"phpfpm_queue", "count", "php-fpm: requests waiting for a worker"},
	{"phpfpm_max_children_reached", "count", "php-fpm: times pm.max_children was hit"},
	{"es_status", "", "elasticsearch: 0 green, 1 yellow, 2 red"},
	{"es_unassigned_shards", "count", "elasticsearch: unassigned shards"},
	{"node_event_loop_lag_ms", "ms", "node.js: event loop lag"},
	{"node_heap_used_bytes", "bytes", "node.js: V8 heap used"},
	{"node_active_handles", "count", "node.js: active handles"},
	// agent itself (kind host)
	{"agent_push_errors", "count", "agent: reports that failed to reach this server, since start"},
	{"agent_collect_errors", "count", "agent: collector runs that failed, since start"},
}

// NumMetrics is the number of slots.
var NumMetrics = len(catalog)

var (
	defs  []plugin.MetricDef
	index map[string]int
)

func init() {
	index = make(map[string]int, len(catalog))
	for i, c := range catalog {
		defs = append(defs, plugin.MetricDef{Name: c.name, Unit: c.unit, Description: c.desc,
			Kind: "gauge", RetentionClass: plugin.RetentionStandard})
		index[c.name] = i
	}
}

// Slot is the slot of a metric name.
func Slot(name string) (int, bool) { i, ok := index[name]; return i, ok }

// Metrics is the layout.
func Metrics() []plugin.MetricDef { return defs }

// Config of a host.agent check: which agent's reports it receives.
type Config struct {
	AgentID     string `json:"agent_id" jsonschema:"required,maxLength=128,description=The agent's ID (AGENT_ID of the agent; a container or host name works). Unique on the server; every report must carry it"`
	MissedCount int    `json:"missed_count,omitempty" jsonschema:"minimum=1,maximum=100,default=3,description=CRITICAL after this many intervals without a report"`
}

var defaults = Config{MissedCount: 3}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Agent implements host.agent.
type Agent struct{}

func (*Agent) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type: "host.agent",
		Kind: plugin.KindIngester,
		Description: "The monitoring agent (cmd/agent), baked into a Docker image or run beside it, POSTing to the ingest URL with the " +
			"check's token: the host or container, the containers it watches, process/port/HTTP probes and services (postgres, redis, nginx, " +
			"php-fpm, elasticsearch, node.js ...) as their own objects. CRITICAL when no report arrives for missed_count intervals.",
		ConfigSchema:    plugin.SchemaFor[Config](),
		DefaultInterval: 30 * time.Second,
		MinInterval:     5 * time.Second,
		BillingClass:    plugin.BillingStandard,
		Objects:         true,
	}
}

func (*Agent) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, defaults)
	if err != nil {
		return err
	}
	if !idPattern.MatchString(cfg.AgentID) {
		return errors.New("agent_id: letters, digits, . _ -, starting with a letter or digit, at most 128 characters")
	}
	if cfg.MissedCount < 1 || cfg.MissedCount > 100 {
		return errors.New("missed_count must be between 1 and 100")
	}
	return nil
}

func (*Agent) MetricsFor(json.RawMessage) ([]plugin.MetricDef, error) { return defs, nil }

// Report is the agent's message.
type Report struct {
	V            int      `json:"v"`
	AgentID      string   `json:"agent_id"`
	Seq          uint64   `json:"seq"`
	Timestamp    float64  `json:"ts"` // Unix seconds
	AgentVersion string   `json:"agent_version"`
	Objects      []Object `json:"objects"`
}

// Object is one reported thing.
type Object struct {
	Key     string             `json:"key"`
	Kind    string             `json:"kind"` // host, container, process, tcp, http, service
	Name    string             `json:"name"`
	Status  string             `json:"status"` // ok, warning, critical, unknown; empty = ok
	Output  string             `json:"output"`
	Labels  map[string]string  `json:"labels"`
	Metrics map[string]float64 `json:"metrics"`
}

// Decode reads a report.
func Decode(body []byte) (Report, error) {
	var r Report
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&r); err != nil {
		return Report{}, fmt.Errorf("not a host.agent report: %w", err)
	}
	if !idPattern.MatchString(r.AgentID) {
		return Report{}, errors.New("agent_id is missing or malformed")
	}
	if len(r.Objects) > maxObjects {
		return Report{}, fmt.Errorf("at most %d objects", maxObjects)
	}
	seen := make(map[string]bool, len(r.Objects))
	for i, o := range r.Objects {
		if o.Key == "" || len(o.Key) > maxKeyBytes {
			return Report{}, fmt.Errorf("object %d: key is missing or longer than %d bytes", i, maxKeyBytes)
		}
		if seen[o.Key] {
			return Report{}, fmt.Errorf("object key %q appears twice", o.Key)
		}
		seen[o.Key] = true
		if _, err := parseStatus(o.Status); err != nil {
			return Report{}, fmt.Errorf("object %q: %w", o.Key, err)
		}
	}
	return r, nil
}

func parseStatus(s string) (plugin.Status, error) {
	switch strings.ToLower(s) {
	case "", "ok":
		return plugin.OK, nil
	case "warning":
		return plugin.Warning, nil
	case "critical":
		return plugin.Critical, nil
	case "unknown":
		return plugin.Unknown, nil
	}
	return 0, fmt.Errorf("status %q is not ok, warning, critical or unknown", s)
}

// worse orders statuses by severity: OK < UNKNOWN < WARNING < CRITICAL.
func worse(a, b plugin.Status) bool { return rank(a) > rank(b) }

func rank(s plugin.Status) int {
	switch s {
	case plugin.OK:
		return 0
	case plugin.Unknown:
		return 1
	case plugin.Warning:
		return 2
	}
	return 3
}

// Convert turns a report into plugin objects, the worst status among them,
// and the number of metric values dropped (unknown name, NaN or infinite).
func Convert(r Report) (objs []plugin.Object, worst plugin.Status, dropped int) {
	for _, o := range r.Objects {
		st, _ := parseStatus(o.Status)
		if worse(st, worst) {
			worst = st
		}
		po := plugin.Object{
			Key:     o.Key,
			Name:    clip(firstNonEmpty(o.Name, o.Key), maxNameBytes),
			Status:  st,
			Output:  clip(o.Output, maxOutput),
			Metrics: plugin.NaNs(NumMetrics),
			Labels:  map[string]string{},
		}
		if o.Kind != "" {
			po.Labels["kind"] = clip(o.Kind, 32)
		}
		for k, v := range o.Labels {
			if len(po.Labels) >= maxLabels {
				break
			}
			if k != "kind" {
				po.Labels[clip(k, 64)] = clip(v, 200)
			}
		}
		for name, v := range o.Metrics {
			slot, ok := index[name]
			if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
				dropped++
				continue
			}
			po.Metrics[slot] = v
		}
		objs = append(objs, po)
	}
	return objs, worst, dropped
}

// Parse makes host.agent an Ingester. The report's agent_id must be the
// check's, so one agent's token cannot report for another.
func (*Agent) Parse(raw json.RawMessage, body []byte) ([]plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(raw, defaults)
	if err != nil {
		return nil, err
	}
	r, err := Decode(body)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(r.AgentID, cfg.AgentID) {
		return nil, errors.New("agent_id is not this check's agent")
	}
	objs, worst, dropped := Convert(r)
	var when time.Time
	if ts := r.Timestamp; ts > 0 && !math.IsInf(ts, 0) && ts < 1e11 {
		sec, frac := math.Modf(ts)
		when = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	}
	return []plugin.Result{{Status: worst, Time: when, Objects: objs, Output: summary(objs, worst, dropped)}}, nil
}

func summary(objs []plugin.Object, worst plugin.Status, dropped int) string {
	bad := 0
	first := ""
	for _, o := range objs {
		if o.Status == plugin.Critical || o.Status == plugin.Warning {
			if first == "" {
				first = o.Name
				if o.Output != "" {
					first += ": " + o.Output
				}
			}
			bad++
		}
	}
	s := fmt.Sprintf("%d objects", len(objs))
	if bad > 0 {
		s += fmt.Sprintf(", %d %s (%s)", bad, strings.ToLower(worst.String()), first)
	}
	if dropped > 0 {
		s += fmt.Sprintf(", %d unknown metric values dropped", dropped)
	}
	return clip(s, plugin.MaxOutput)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func clip(s string, n int) string {
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "")
	}
	return s
}
