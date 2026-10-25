// Package docker watches the containers of a Docker engine through its API
// socket: state, health, restarts, exit codes, CPU, memory and network. It
// talks plain HTTP over the unix socket, so the agent carries no SDK.
//
// Mount the socket read-only into the agent's container
// (-v /var/run/docker.sock:/var/run/docker.sock:ro). Anything that can read
// the socket can read every container's environment; run the agent as a
// sidecar for it, not inside application images.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

func init() {
	agent.Register("docker", func(o agent.Options) (agent.Collector, error) {
		return New(o.String("socket", "/var/run/docker.sock"), o.String("label", ""))
	})
}

// maxParallel bounds concurrent stats calls to the engine.
const maxParallel = 8

// Collector implements agent.Collector.
type Collector struct {
	client *http.Client
	label  string // optional "key" or "key=value" filter
	now    func() time.Time

	mu   sync.Mutex
	prev map[string]sample // by container ID
	seen map[string]bool   // IDs seen running: they stay reported when they stop
}

type sample struct {
	at               time.Time
	cpuTotal, cpuSys float64
	netRx, netTx     float64
}

// New builds a collector for the engine socket; label limits it to
// containers with that label ("monitor.enable=true"), empty means all.
//
// A stopped container is reported (critical, or a warning after a clean
// exit) when the label selects it, or when the agent saw it running: a
// crash must alert. Without a label, containers that were already stopped
// when the agent started are left out, so old exited containers are not
// incidents.
func New(socket, label string) (*Collector, error) {
	if socket == "" {
		return nil, fmt.Errorf("socket is empty")
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Collector{client: &http.Client{Transport: tr, Timeout: 10 * time.Second}, label: label, now: time.Now,
		prev: map[string]sample{}, seen: map[string]bool{}}, nil
}

type listItem struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

type inspect struct {
	RestartCount int `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

type stats struct {
	CPU struct {
		Usage struct {
			Total float64 `json:"total_usage"`
		} `json:"cpu_usage"`
		System float64 `json:"system_cpu_usage"`
		Online float64 `json:"online_cpus"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage float64            `json:"usage"`
		Limit float64            `json:"limit"`
		Stats map[string]float64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		Rx float64 `json:"rx_bytes"`
		Tx float64 `json:"tx_bytes"`
	} `json:"networks"`
}

func (c *Collector) get(ctx context.Context, path string, q url.Values, out any) error {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Collector) Collect(ctx context.Context) ([]agent.Object, error) {
	q := url.Values{"all": {"1"}}
	if c.label != "" {
		f, _ := json.Marshal(map[string][]string{"label": {c.label}})
		q.Set("filters", string(f))
	}
	var list []listItem
	if err := c.get(ctx, "/containers/json", q, &list); err != nil {
		// The engine itself is the thing to report: it is down or the
		// socket is not mounted.
		return []agent.Object{{Key: "docker:engine", Kind: "service", Name: "docker", Status: agent.Critical,
			Output: err.Error(), Metrics: map[string]float64{"up": 0}}}, nil
	}
	objs := make([]agent.Object, len(list))
	keep := make([]bool, len(list))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	now := c.now()
	for i, it := range list {
		if it.Labels["monitor.ignore"] == "true" {
			continue
		}
		if !c.wanted(it) {
			continue
		}
		keep[i] = true
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			objs[i] = c.one(ctx, it, now)
		}()
	}
	wg.Wait()

	out := []agent.Object{{Key: "docker:engine", Kind: "service", Name: "docker", Metrics: map[string]float64{"up": 1}}}
	live := map[string]bool{}
	for i, o := range objs {
		if keep[i] {
			out = append(out, o)
			live[list[i].ID] = true
		}
	}
	c.mu.Lock()
	for id := range c.prev {
		if !live[id] {
			delete(c.prev, id)
		}
	}
	for id := range c.seen {
		if !live[id] {
			delete(c.seen, id) // removed from the engine
		}
	}
	c.mu.Unlock()
	sort.Slice(out[1:], func(a, b int) bool { return out[1+a].Key < out[1+b].Key })
	return out, nil
}

// wanted says whether a listed container is reported, and remembers the
// running ones.
func (c *Collector) wanted(it listItem) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it.State == "running" || it.State == "restarting" {
		c.seen[it.ID] = true
		return true
	}
	return c.label != "" || c.seen[it.ID]
}

func (c *Collector) one(ctx context.Context, it listItem, now time.Time) agent.Object {
	name := strings.TrimPrefix(first(it.Names), "/")
	if name == "" {
		name = it.ID[:12]
	}
	o := agent.Object{Key: "container:" + name, Kind: "container", Name: name,
		Labels:  map[string]string{"image": it.Image, "id": it.ID[:12], "state": it.State},
		Metrics: map[string]float64{"container_running": 0}}
	for _, k := range []string{"com.docker.compose.project", "com.docker.compose.service"} {
		if v := it.Labels[k]; v != "" {
			o.Labels[strings.TrimPrefix(k, "com.docker.compose.")] = v
		}
	}
	var in inspect
	if err := c.get(ctx, "/containers/"+it.ID+"/json", nil, &in); err != nil {
		o.Status, o.Output = agent.Unknown, err.Error()
		return o
	}
	m := o.Metrics
	m["container_restarts"] = float64(in.RestartCount)
	if in.State.Running {
		m["container_healthy"] = 1
		if h := in.State.Health; h != nil {
			o.Labels["health"] = h.Status
			if h.Status == "unhealthy" {
				m["container_healthy"] = 0
			}
		}
	}
	switch {
	case in.State.Restarting:
		o.Status, o.Output = agent.Warning, fmt.Sprintf("restarting (exit code %d)", in.State.ExitCode)
		m["container_exit_code"] = float64(in.State.ExitCode)
	case !in.State.Running:
		m["container_exit_code"] = float64(in.State.ExitCode)
		o.Status, o.Output = agent.Critical, fmt.Sprintf("%s, exit code %d", in.State.Status, in.State.ExitCode)
		if in.State.OOMKilled {
			o.Output += " (OOM killed)"
		}
		if in.State.ExitCode == 0 && in.State.Status == "exited" {
			o.Status = agent.Warning
		}
		return o
	default:
		m["container_running"] = 1
		if t, err := time.Parse(time.RFC3339Nano, in.State.StartedAt); err == nil {
			m["uptime_seconds"] = max(now.Sub(t).Seconds(), 0)
		}
		if m["container_healthy"] == 0 {
			o.Status, o.Output = agent.Critical, "health check fails"
		}
	}
	if in.State.Running {
		c.addStats(ctx, it.ID, m, now)
	}
	return o
}

func (c *Collector) addStats(ctx context.Context, id string, m map[string]float64, now time.Time) {
	var s stats
	if err := c.get(ctx, "/containers/"+id+"/stats", url.Values{"stream": {"false"}, "one-shot": {"true"}}, &s); err != nil {
		return
	}
	cache := s.Memory.Stats["inactive_file"] // cgroup v2
	if v, ok := s.Memory.Stats["total_inactive_file"]; ok {
		cache = v // cgroup v1
	}
	if used := max(s.Memory.Usage-cache, 0); s.Memory.Usage > 0 {
		m["mem_used_bytes"] = used
		if s.Memory.Limit > 0 {
			m["mem_total_bytes"] = s.Memory.Limit
			m["mem_usage_pct"] = min(used/s.Memory.Limit*100, 100)
		}
	}
	cur := sample{at: now, cpuTotal: s.CPU.Usage.Total, cpuSys: s.CPU.System}
	for _, n := range s.Networks {
		cur.netRx += n.Rx
		cur.netTx += n.Tx
	}
	c.mu.Lock()
	p, had := c.prev[id]
	c.prev[id] = cur
	c.mu.Unlock()
	if !had {
		return
	}
	if ds := cur.cpuSys - p.cpuSys; ds > 0 && cur.cpuTotal >= p.cpuTotal {
		cpus := s.CPU.Online
		if cpus == 0 {
			cpus = 1
		}
		m["cpu_usage_pct"] = (cur.cpuTotal - p.cpuTotal) / ds * cpus * 100
	}
	if dt := now.Sub(p.at).Seconds(); dt > 0 {
		m["net_rx_bytes_per_sec"] = max(cur.netRx-p.netRx, 0) / dt
		m["net_tx_bytes_per_sec"] = max(cur.netTx-p.netTx, 0) / dt
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
