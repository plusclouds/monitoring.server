// Package agent is the monitoring agent (F15): a small static binary that
// runs inside a Docker image, or beside it, collects metrics with pluggable
// collectors and POSTs them to the server's ingest listener (plugin
// host.agent).
//
// The agent reports facts and a status; thresholds live on the server.
package agent

import "context"

// Object is one reported thing: the host, a container, a probe, a service.
// Its JSON form is the wire format plugins/hostagent decodes.
type Object struct {
	Key     string             `json:"key"`
	Kind    string             `json:"kind"`
	Name    string             `json:"name,omitempty"`
	Status  string             `json:"status,omitempty"` // ok (default), warning, critical, unknown
	Output  string             `json:"output,omitempty"`
	Labels  map[string]string  `json:"labels,omitempty"`
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

// Report is one POST body.
type Report struct {
	V            int      `json:"v"`
	AgentID      string   `json:"agent_id"`
	Seq          uint64   `json:"seq"`
	Timestamp    float64  `json:"ts"`
	AgentVersion string   `json:"agent_version"`
	Objects      []Object `json:"objects"`
}

// Collector reads one source. A returned error is a problem of the agent or
// its setup (unreadable /proc, bad option) and is reported as an UNKNOWN
// object; a service that does not answer is not an error: the collector
// returns it as an object with up=0 and status critical.
type Collector interface {
	Collect(ctx context.Context) ([]Object, error)
}

// Status values of an Object.
const (
	OK       = "ok"
	Warning  = "warning"
	Critical = "critical"
	Unknown  = "unknown"
)
