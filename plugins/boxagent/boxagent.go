// Package boxagent implements box.agent: the heartbeat the box.agent service
// (a WebSocket proxy in front of vLLM on an LLM host) POSTs to the ingest
// listener every 5 seconds, with a token minted for monitoring alone
// (push token, F08). A box is one check whose objects are the host, the
// vLLM backend and each GPU, so each has its own thresholds, state and graphs.
//
// The agent reports facts and a severity; the server decides. Silence is
// CRITICAL through the last-seen rule (missed_count intervals), so a box that
// is booting, downloading a model or restarting vLLM keeps sending heartbeats
// with that state and is not an incident.
package boxagent

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

// Metric slots, shared by every object; an object fills its own.
const (
	Load1 = iota
	MemUsage
	DiskUsage
	OOMKills
	VLLMUp
	BackendHealthy
	VLLMRestarts
	QueueRunning
	QueueWaiting
	RouterConnected
	GPUTemp
	GPUUtil
	GPUMemUsed
	GPUMemTotal
	GPUMemUsage
	GPUECCDbe
	GPUXid
	GPUThrottle
	Resp2xx
	Resp429
	Resp4xx
	Resp5xx
	RespErrors
	NumMetrics
)

var defs = []plugin.MetricDef{
	g("load1", "", "host: 1-minute load average"),
	g("memory_usage_pct", "percent", "host: memory used"),
	g("disk_usage_pct", "percent", "host: fullest disk"),
	g("oom_kills", "count", "host: OOM kills since boot"),
	g("vllm_up", "", "vllm: 1 process running, 0 not"),
	g("backend_healthy", "", "vllm: 1 health probe passes, 0 fails"),
	g("vllm_restarts", "count", "vllm: supervisor restarts since the agent started"),
	g("queue_running", "count", "vllm: requests running"),
	g("queue_waiting", "count", "vllm: requests waiting"),
	g("router_connected", "", "host: 1 the router WebSocket is up, 0 down"),
	g("gpu_temp_c", "celsius", "gpu: temperature"),
	g("gpu_util_pct", "percent", "gpu: busy"),
	g("gpu_mem_used_bytes", "bytes", "gpu: memory used"),
	g("gpu_mem_total_bytes", "bytes", "gpu: memory installed"),
	g("gpu_mem_usage_pct", "percent", "gpu: memory used"),
	g("gpu_ecc_dbe", "count", "gpu: uncorrectable (double-bit) ECC errors"),
	g("gpu_xid_last", "", "gpu: last XID error code (0 none)"),
	g("gpu_throttle", "", "gpu: 1 throttled by heat or power, 0 not"),
	g("monitor_responses_2xx", "count", "host: heartbeats this server accepted, since the agent started"),
	g("monitor_responses_429", "count", "host: heartbeats refused by the rate limit (429)"),
	g("monitor_responses_4xx", "count", "host: other 4xx answers (bad token, disabled check, invalid message)"),
	g("monitor_responses_5xx", "count", "host: 5xx answers (server busy or failing)"),
	g("monitor_send_errors", "count", "host: heartbeats that got no answer (network, timeout)"),
}

func g(name, unit, desc string) plugin.MetricDef {
	// A 5 s stream for liveness and thresholds; the standard class keeps its
	// storage and downsampling in line with other plugins.
	return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
}

// Config of a box.agent check: which box's heartbeats it receives.
type Config struct {
	BoxID       string `json:"box_id" jsonschema:"required,maxLength=128,description=The box's ID (the agent instance ID llmocean.api uses). Unique on the server; every heartbeat must carry it"`
	MissedCount int    `json:"missed_count,omitempty" jsonschema:"minimum=1,maximum=100,default=3,description=CRITICAL after this many intervals without a heartbeat"`
}

var defaults = Config{MissedCount: 3}

var boxID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Agent implements box.agent.
type Agent struct{}

func (*Agent) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type: "box.agent",
		Kind: plugin.KindIngester,
		Description: "An LLM box's own heartbeat from box.agent (POST to the ingest URL with the check's token, every 5 seconds): " +
			"host, vLLM backend and each GPU as their own objects. CRITICAL when no heartbeat arrives for missed_count intervals, " +
			"or on a hard GPU fault (XID 79, uncorrectable ECC). Boot, model download and vLLM restarts are not incidents.",
		ConfigSchema:      plugin.SchemaFor[Config](),
		DefaultInterval:   5 * time.Second,
		MinInterval:       5 * time.Second,
		TenantFloorExempt: true, // 5 s in every tenant: the agent beats at this rate
		BillingClass:      plugin.BillingStandard,
		Objects:           true,
	}
}

func (*Agent) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, defaults)
	if err != nil {
		return err
	}
	if !boxID.MatchString(cfg.BoxID) {
		return errors.New("box_id: letters, digits, . _ -, starting with a letter or digit, at most 128 characters")
	}
	if cfg.MissedCount < 1 || cfg.MissedCount > 100 {
		return errors.New("missed_count must be between 1 and 100")
	}
	return nil
}

func (*Agent) MetricsFor(json.RawMessage) ([]plugin.MetricDef, error) { return defs, nil }

// Metrics is the layout.
func Metrics() []plugin.MetricDef { return defs }

// MissedCount is the number of silent intervals before CRITICAL.
func MissedCount(raw json.RawMessage) int {
	cfg, err := plugin.DecodeConfig(raw, defaults)
	if err != nil || cfg.MissedCount < 1 {
		return defaults.MissedCount
	}
	return cfg.MissedCount
}

// Heartbeat is the agent's message.
type Heartbeat struct {
	V              int     `json:"v"`
	BoxID          string  `json:"box_id"`
	Seq            uint64  `json:"seq"`
	Timestamp      float64 `json:"ts"` // Unix seconds
	AgentVersion   string  `json:"agent_version"`
	State          string  `json:"state"`    // downloading, booting, ready, unhealthy, restarting, stopped
	Severity       string  `json:"severity"` // ok, degraded, failed
	BackendHealthy *bool   `json:"backend_healthy"`
	RouterUp       *bool   `json:"router_connected"`
	VLLM           *struct {
		Up       *bool    `json:"up"`
		Restarts *float64 `json:"restart_count"`
		Reason   string   `json:"last_restart_reason"`
	} `json:"vllm"`
	GPU []struct {
		Idx      *int     `json:"idx"`
		Temp     *float64 `json:"temp"`
		Util     *float64 `json:"util"`
		MemUsed  *float64 `json:"mem_used"`
		MemTotal *float64 `json:"mem_total"`
		ECCDbe   *float64 `json:"ecc_dbe"`
		XID      *float64 `json:"xid_last"`
		Throttle *bool    `json:"throttle"`
	} `json:"gpu"`
	// Responses are the agent's own count of this server's answers since it
	// started; they reach the server on a later beat.
	Responses *struct {
		R2xx   *float64 `json:"2xx"`
		R429   *float64 `json:"429"`
		R4xx   *float64 `json:"4xx"`
		R5xx   *float64 `json:"5xx"`
		Errors *float64 `json:"errors"`
	} `json:"monitor_responses"`
	Queue *struct {
		Running *float64 `json:"running"`
		Waiting *float64 `json:"waiting"`
	} `json:"queue"`
	Host *struct {
		Load    *float64 `json:"load"`
		MemPct  *float64 `json:"mem_pct"`
		DiskPct *float64 `json:"disk_pct"`
		OOM     *float64 `json:"oom_kills"`
	} `json:"host"`
}

// Message is one decoded heartbeat.
type Message struct {
	Box  string // box_id
	Time time.Time
	HB   Heartbeat
}

// Limits against a broken or hostile agent.
const maxGPUs = 64

// Decode reads a heartbeat.
func Decode(body []byte) (Message, error) {
	var hb Heartbeat
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&hb); err != nil {
		return Message{}, fmt.Errorf("not a box.agent heartbeat: %w", err)
	}
	if !boxID.MatchString(hb.BoxID) {
		return Message{}, errors.New("box_id is missing or malformed")
	}
	switch hb.Severity {
	case "", "ok", "degraded", "failed":
	default:
		return Message{}, fmt.Errorf("severity %q is not ok, degraded or failed", hb.Severity)
	}
	if len(hb.GPU) > maxGPUs {
		return Message{}, fmt.Errorf("at most %d GPUs", maxGPUs)
	}
	m := Message{Box: hb.BoxID, HB: hb}
	if ts := hb.Timestamp; ts > 0 && !math.IsInf(ts, 0) && ts < 1e11 {
		sec, frac := math.Modf(ts)
		m.Time = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	}
	return m, nil
}

// transient states are expected to look unhealthy: a box in one of them is
// not an incident while the heartbeats keep coming. "stopped" is the last
// beat of a graceful shutdown; the silence after it is still CRITICAL unless
// the check is disabled or deleted.
func transient(state string) bool {
	switch state {
	case "downloading", "booting", "restarting", "stopped":
		return true
	}
	return false
}

// hardXID are the XID codes that mean the GPU is gone or its memory is
// broken: 79 fell off the bus, 48 double-bit ECC, 64 ECC row-remap failure.
func hardXID(x float64) bool { return x == 79 || x == 48 || x == 64 }

// HardFault names the first GPU fault that justifies replacing the box at
// once, or "".
func HardFault(m Message) string {
	for i, gpu := range m.HB.GPU {
		idx := i
		if gpu.Idx != nil {
			idx = *gpu.Idx
		}
		if gpu.XID != nil && hardXID(*gpu.XID) {
			return fmt.Sprintf("GPU %d XID %d", idx, int(*gpu.XID))
		}
		if gpu.ECCDbe != nil && *gpu.ECCDbe > 0 {
			return fmt.Sprintf("GPU %d uncorrectable ECC errors (%d)", idx, int(*gpu.ECCDbe))
		}
	}
	return ""
}

// Objects turns a heartbeat into objects: host, vllm and one per GPU.
func Objects(m Message) []plugin.Object {
	hb := m.HB
	host := plugin.Object{Key: "host", Name: "host", Labels: map[string]string{"kind": "host"}, Metrics: plugin.NaNs(NumMetrics)}
	if h := hb.Host; h != nil {
		set(host.Metrics, Load1, h.Load)
		set(host.Metrics, MemUsage, h.MemPct)
		set(host.Metrics, DiskUsage, h.DiskPct)
		set(host.Metrics, OOMKills, h.OOM)
	}
	setBool(host.Metrics, RouterConnected, hb.RouterUp)
	if r := hb.Responses; r != nil {
		set(host.Metrics, Resp2xx, r.R2xx)
		set(host.Metrics, Resp429, r.R429)
		set(host.Metrics, Resp4xx, r.R4xx)
		set(host.Metrics, Resp5xx, r.R5xx)
		set(host.Metrics, RespErrors, r.Errors)
	}

	vllm := plugin.Object{Key: "vllm", Name: "vllm", Labels: map[string]string{"kind": "backend", "state": hb.State},
		Metrics: plugin.NaNs(NumMetrics)}
	setBool(vllm.Metrics, BackendHealthy, hb.BackendHealthy)
	if v := hb.VLLM; v != nil {
		setBool(vllm.Metrics, VLLMUp, v.Up)
		set(vllm.Metrics, VLLMRestarts, v.Restarts)
		if v.Reason != "" {
			vllm.Labels["last_restart_reason"] = truncate(v.Reason)
		}
	}
	if q := hb.Queue; q != nil {
		set(vllm.Metrics, QueueRunning, q.Running)
		set(vllm.Metrics, QueueWaiting, q.Waiting)
	}
	if !transient(hb.State) {
		switch {
		case hb.VLLM != nil && hb.VLLM.Up != nil && !*hb.VLLM.Up:
			vllm.Status, vllm.Output = plugin.Critical, "vLLM is not running"
		case hb.BackendHealthy != nil && !*hb.BackendHealthy:
			vllm.Status, vllm.Output = plugin.Critical, "vLLM health probe fails"
		}
	}

	out := []plugin.Object{host, vllm}
	for i, gpu := range hb.GPU {
		idx := i
		if gpu.Idx != nil && *gpu.Idx >= 0 && *gpu.Idx < 1<<16 {
			idx = *gpu.Idx
		}
		o := plugin.Object{Key: fmt.Sprintf("gpu:%d", idx), Name: fmt.Sprintf("GPU %d", idx),
			Labels: map[string]string{"kind": "gpu"}, Metrics: plugin.NaNs(NumMetrics)}
		set(o.Metrics, GPUTemp, gpu.Temp)
		set(o.Metrics, GPUUtil, gpu.Util)
		set(o.Metrics, GPUMemUsed, gpu.MemUsed)
		set(o.Metrics, GPUMemTotal, gpu.MemTotal)
		set(o.Metrics, GPUECCDbe, gpu.ECCDbe)
		set(o.Metrics, GPUXid, gpu.XID)
		setBool(o.Metrics, GPUThrottle, gpu.Throttle)
		if gpu.MemUsed != nil && gpu.MemTotal != nil && finite(*gpu.MemUsed) && finite(*gpu.MemTotal) && *gpu.MemTotal > 0 {
			o.Metrics[GPUMemUsage] = *gpu.MemUsed / *gpu.MemTotal * 100
		}
		if gpu.XID != nil && hardXID(*gpu.XID) {
			o.Status, o.Output = plugin.Critical, fmt.Sprintf("XID %d", int(*gpu.XID))
		} else if gpu.ECCDbe != nil && *gpu.ECCDbe > 0 {
			o.Status, o.Output = plugin.Critical, "uncorrectable ECC errors"
		}
		out = append(out, o)
	}
	return out
}

// Status is the check's state for a heartbeat: a hard GPU fault is CRITICAL
// whatever the state; transient states are OK; otherwise the agent's
// severity, with a lost router connection on a ready box a WARNING.
func Status(m Message) (plugin.Status, string) {
	if f := HardFault(m); f != "" {
		return plugin.Critical, f
	}
	hb := m.HB
	if transient(hb.State) {
		return plugin.OK, "box is " + hb.State
	}
	switch hb.Severity {
	case "failed":
		return plugin.Critical, "agent reports failed (state " + hb.State + ")"
	case "degraded":
		return plugin.Warning, "agent reports degraded (state " + hb.State + ")"
	}
	if hb.State == "unhealthy" {
		return plugin.Critical, "box is unhealthy"
	}
	if hb.RouterUp != nil && !*hb.RouterUp {
		return plugin.Warning, "router connection is down"
	}
	return plugin.OK, ""
}

// Summary is the check's output for a message.
func Summary(m Message, why string) string {
	parts := []string{fmt.Sprintf("state %s", orDash(m.HB.State))}
	if why != "" {
		parts = append(parts, why)
	}
	parts = append(parts, fmt.Sprintf("%d GPUs", len(m.HB.GPU)))
	return strings.Join(parts, ", ")
}

// Parse makes box.agent an Ingester. The heartbeat's box_id must be the
// check's, so one box's token cannot report for another.
func (*Agent) Parse(raw json.RawMessage, body []byte) ([]plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(raw, defaults)
	if err != nil {
		return nil, err
	}
	m, err := Decode(body)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(m.Box, cfg.BoxID) {
		return nil, errors.New("box_id is not this check's box")
	}
	status, why := Status(m)
	return []plugin.Result{{Status: status, Time: m.Time, Objects: Objects(m), Output: Summary(m, why)}}, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func set(dst []float64, slot int, v *float64) {
	if v != nil && finite(*v) {
		dst[slot] = *v
	}
}

func setBool(dst []float64, slot int, v *bool) {
	if v == nil {
		return
	}
	dst[slot] = 0
	if *v {
		dst[slot] = 1
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
