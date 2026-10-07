// Package vmagent implements vm.agent: the telemetry PlusClouds' VM agent
// (github.com/plusclouds/ubuntu-agent) publishes on NATS every 30 s,
// received by the ingest role. A VM is one check whose objects are the
// host, each disk (by mountpoint) and each network interface, so disks and
// interfaces have their own thresholds, state and graphs.
package vmagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Agent{}) }

// Metric slots, shared by every object; an object fills its own.
const (
	CPUUsage = iota
	CPUCores
	Load1
	Load5
	Load15
	MemUsage
	MemUsed
	MemTotal
	DiskUsage
	DiskUsed
	DiskTotal
	DiskReadBps
	DiskWriteBps
	DiskReadIOPS
	DiskWriteIOPS
	DiskUtil
	NetUp
	NetRxBps
	NetTxBps
	NumMetrics
)

var defs = []plugin.MetricDef{
	g("cpu_usage_pct", "percent", "host: CPU busy"),
	g("cpu_cores", "count", "host: cores"),
	g("load1", "", "host: 1-minute load average (0 on Windows)"),
	g("load5", "", "host: 5-minute load average"),
	g("load15", "", "host: 15-minute load average"),
	g("memory_usage_pct", "percent", "host: memory used"),
	g("memory_used_bytes", "bytes", "host: memory used"),
	g("memory_total_bytes", "bytes", "host: memory installed"),
	g("disk_usage_pct", "percent", "disk: space used"),
	g("disk_used_bytes", "bytes", "disk: space used"),
	g("disk_total_bytes", "bytes", "disk: size"),
	g("disk_read_bytes_per_s", "bytes/s", "disk: read rate"),
	g("disk_write_bytes_per_s", "bytes/s", "disk: write rate"),
	g("disk_read_iops", "1/s", "disk: read operations"),
	g("disk_write_iops", "1/s", "disk: write operations"),
	g("disk_util_pct", "percent", "disk: busy time"),
	g("net_up", "", "interface: 1 up, 0 down"),
	g("net_rx_bits_per_s", "bit/s", "interface: received, from the counters' difference"),
	g("net_tx_bits_per_s", "bit/s", "interface: sent, from the counters' difference"),
}

func g(name, unit, desc string) plugin.MetricDef {
	return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
}

// Config of a vm.agent check: which VM's telemetry it receives.
type Config struct {
	VMUUID string `json:"vm_uuid" jsonschema:"required,description=The VM's UUID: its agent publishes on vm.<uuid>.telemetry. Unique on the server"`
}

// Agent implements vm.agent.
type Agent struct{}

func (*Agent) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type: "vm.agent",
		Kind: plugin.KindIngester,
		Description: "A PlusClouds VM's own telemetry from its agent (over the platform's NATS, every 30 s): CPU, " +
			"memory and load as the \"host\" object, each disk and network interface as its own object. " +
			"CRITICAL when no telemetry arrives for missed_count intervals (VM off or agent stopped).",
		ConfigSchema:    plugin.SchemaFor[Config](),
		DefaultInterval: time.Minute,
		MinInterval:     30 * time.Second,
		BillingClass:    plugin.BillingStandard,
		Objects:         true,
	}
}

func (*Agent) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, Config{})
	if err != nil {
		return err
	}
	if _, err := uuid.Parse(cfg.VMUUID); err != nil {
		return errors.New("vm_uuid must be the VM's UUID")
	}
	return nil
}

func (*Agent) MetricsFor(json.RawMessage) ([]plugin.MetricDef, error) { return defs, nil }

// Metrics is the layout.
func Metrics() []plugin.MetricDef { return defs }

// Envelope is the agent's message (internal/protocol/envelope.go).
type Envelope struct {
	V         int             `json:"v"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	AgentType string          `json:"agent_type"`
	AgentUUID string          `json:"agent_uuid"`
	Timestamp float64         `json:"timestamp"` // Unix seconds
	Payload   json.RawMessage `json:"payload"`
}

// Telemetry is the agent's system.SystemMetrics.
type Telemetry struct {
	CPU struct {
		UsagePct  *float64  `json:"usage_pct"`
		CoreCount *float64  `json:"core_count"`
		LoadAvg   []float64 `json:"load_avg"`
	} `json:"cpu"`
	Memory struct {
		TotalBytes *float64 `json:"total_bytes"`
		UsedBytes  *float64 `json:"used_bytes"`
		UsagePct   *float64 `json:"usage_pct"`
	} `json:"memory"`
	Disks []struct {
		Device     string   `json:"device"`
		Mountpoint string   `json:"mountpoint"`
		TotalBytes *float64 `json:"total_bytes"`
		UsedBytes  *float64 `json:"used_bytes"`
		UsagePct   *float64 `json:"usage_pct"`
		IO         *struct {
			ReadBps   *float64 `json:"read_bytes_per_s"`
			WriteBps  *float64 `json:"write_bytes_per_s"`
			ReadIOPS  *float64 `json:"read_iops"`
			WriteIOPS *float64 `json:"write_iops"`
			UtilPct   *float64 `json:"util_pct"`
		} `json:"io"`
	} `json:"disks"`
	Network []struct {
		Interface string   `json:"interface"`
		BytesSent *float64 `json:"bytes_sent"`
		BytesRecv *float64 `json:"bytes_recv"`
		IsUp      *bool    `json:"is_up"`
	} `json:"network"`
}

// Message is one decoded telemetry message.
type Message struct {
	VM        string // agent_uuid, lower-case
	Time      time.Time
	Telemetry Telemetry
}

// Limits against a broken or hostile agent.
const (
	maxObjects = 200
	maxKey     = 200
)

// Decode reads a telemetry envelope; other message types are an error.
func Decode(body []byte) (Message, error) {
	var env Envelope
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&env); err != nil {
		return Message{}, fmt.Errorf("not an agent envelope: %w", err)
	}
	if env.Type != "telemetry" {
		return Message{}, fmt.Errorf("message type %q is not telemetry", env.Type)
	}
	if _, err := uuid.Parse(env.AgentUUID); err != nil {
		return Message{}, errors.New("agent_uuid is not a UUID")
	}
	m := Message{VM: strings.ToLower(env.AgentUUID)}
	if env.Timestamp > 0 && !math.IsInf(env.Timestamp, 0) && env.Timestamp < 1e11 {
		sec, frac := math.Modf(env.Timestamp)
		m.Time = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	}
	if err := json.Unmarshal(env.Payload, &m.Telemetry); err != nil {
		return Message{}, fmt.Errorf("telemetry payload: %w", err)
	}
	return m, nil
}

// Counters are an interface's byte counters at a time, to turn the next
// message's counters into rates.
type Counters struct {
	At       time.Time
	Sent     float64
	Received float64
}

// Objects turns telemetry into objects. prev holds the interfaces'
// counters from the previous message (updated in place); rates need two
// messages and a counter that did not go back (reboot).
func Objects(m Message, prev map[string]Counters) []plugin.Object {
	t := m.Telemetry
	host := plugin.Object{Key: "host", Name: "host", Labels: map[string]string{"kind": "host"}, Metrics: plugin.NaNs(NumMetrics)}
	set(host.Metrics, CPUUsage, t.CPU.UsagePct)
	set(host.Metrics, CPUCores, t.CPU.CoreCount)
	for i, slot := range []int{Load1, Load5, Load15} {
		if i < len(t.CPU.LoadAvg) && finite(t.CPU.LoadAvg[i]) {
			host.Metrics[slot] = t.CPU.LoadAvg[i]
		}
	}
	set(host.Metrics, MemUsage, t.Memory.UsagePct)
	set(host.Metrics, MemUsed, t.Memory.UsedBytes)
	set(host.Metrics, MemTotal, t.Memory.TotalBytes)
	out := []plugin.Object{host}

	for _, d := range t.Disks {
		if d.Mountpoint == "" || len(d.Mountpoint) > maxKey || len(out) >= maxObjects {
			continue
		}
		o := plugin.Object{Key: "disk:" + d.Mountpoint, Name: d.Mountpoint,
			Labels: map[string]string{"kind": "disk", "device": truncate(d.Device)}, Metrics: plugin.NaNs(NumMetrics)}
		set(o.Metrics, DiskUsage, d.UsagePct)
		set(o.Metrics, DiskUsed, d.UsedBytes)
		set(o.Metrics, DiskTotal, d.TotalBytes)
		if d.IO != nil {
			set(o.Metrics, DiskReadBps, d.IO.ReadBps)
			set(o.Metrics, DiskWriteBps, d.IO.WriteBps)
			set(o.Metrics, DiskReadIOPS, d.IO.ReadIOPS)
			set(o.Metrics, DiskWriteIOPS, d.IO.WriteIOPS)
			set(o.Metrics, DiskUtil, d.IO.UtilPct)
		}
		out = append(out, o)
	}

	seen := map[string]bool{}
	for _, n := range t.Network {
		if n.Interface == "" || len(n.Interface) > maxKey || len(out) >= maxObjects {
			continue
		}
		key := "net:" + n.Interface
		seen[key] = true
		o := plugin.Object{Key: key, Name: n.Interface, Labels: map[string]string{"kind": "interface"},
			Metrics: plugin.NaNs(NumMetrics)}
		if n.IsUp != nil {
			o.Metrics[NetUp] = 0
			if *n.IsUp {
				o.Metrics[NetUp] = 1
			} else {
				o.Status, o.Output = plugin.Warning, n.Interface+" is down"
			}
		}
		if n.BytesSent != nil && n.BytesRecv != nil && prev != nil && !m.Time.IsZero() {
			cur := Counters{At: m.Time, Sent: *n.BytesSent, Received: *n.BytesRecv}
			if p, ok := prev[key]; ok {
				secs := cur.At.Sub(p.At).Seconds()
				if secs > 0 && secs < 3600 && cur.Sent >= p.Sent && cur.Received >= p.Received {
					o.Metrics[NetTxBps] = (cur.Sent - p.Sent) * 8 / secs
					o.Metrics[NetRxBps] = (cur.Received - p.Received) * 8 / secs
				}
			}
			prev[key] = cur
		}
		out = append(out, o)
	}
	for k := range prev {
		if !seen[k] {
			delete(prev, k)
		}
	}
	return out
}

// Summary is the check's output for a message.
func Summary(m Message, objects []plugin.Object) string {
	t := m.Telemetry
	parts := []string{}
	if t.CPU.UsagePct != nil {
		parts = append(parts, fmt.Sprintf("CPU %.1f %%", *t.CPU.UsagePct))
	}
	if t.Memory.UsagePct != nil {
		parts = append(parts, fmt.Sprintf("memory %.1f %%", *t.Memory.UsagePct))
	}
	var disks, nets, down []string
	for _, o := range objects {
		switch o.Labels["kind"] {
		case "disk":
			disks = append(disks, o.Name)
		case "interface":
			nets = append(nets, o.Name)
			if o.Status == plugin.Warning {
				down = append(down, o.Name)
			}
		}
	}
	parts = append(parts, fmt.Sprintf("%d disks, %d interfaces", len(disks), len(nets)))
	if len(down) > 0 {
		slices.Sort(down)
		parts = append(parts, "down: "+strings.Join(down, ", "))
	}
	return strings.Join(parts, ", ")
}

// Parse makes vm.agent an Ingester for one envelope, without interface
// rates (they need the previous message; the NATS consumer keeps it).
func (*Agent) Parse(_ json.RawMessage, body []byte) ([]plugin.Result, error) {
	m, err := Decode(body)
	if err != nil {
		return nil, err
	}
	objs := Objects(m, nil)
	return []plugin.Result{{Status: plugin.OK, Time: m.Time, Objects: objs, Output: Summary(m, objs)}}, nil
}

func set(dst []float64, slot int, v *float64) {
	if v != nil && finite(*v) {
		dst[slot] = *v
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func truncate(s string) string {
	if len(s) > maxKey {
		return s[:maxKey]
	}
	return s
}
