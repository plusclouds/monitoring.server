package services

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

type node struct {
	name, url, metricsURL string
	client                *http.Client
	cpu                   rate
	now                   func() time.Time
}

func newNode(o agent.Options) (agent.Collector, error) {
	to, err := o.Duration("timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	n := &node{name: o.Name, url: o.String("url", ""), metricsURL: o.String("metrics_url", ""),
		client: httpClient(to, o.String("insecure_skip_verify", "") == "true"), now: time.Now}
	if n.url == "" && n.metricsURL == "" {
		return nil, fmt.Errorf("set url (a health endpoint) and/or metrics_url (Prometheus text, e.g. from prom-client)")
	}
	for _, u := range []string{n.url, n.metricsURL} {
		if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return nil, fmt.Errorf("%q must start with http:// or https://", u)
		}
	}
	return n, nil
}

// Collect checks the app's health endpoint and, when metrics_url is set,
// reads the default metrics of prom-client (event loop lag, heap, handles,
// memory, CPU). A Node.js app needs no agent code inside it.
func (n *node) Collect(ctx context.Context) ([]agent.Object, error) {
	obj := object("nodejs", n.name, nil)
	probe := n.url
	if probe == "" {
		probe = n.metricsURL
	}
	start := time.Now()
	body, status, err := n.fetch(ctx, probe)
	if err != nil {
		return down(obj, err), nil
	}
	m := obj.Metrics
	m["up"], m["response_ms"], m["http_status"] = 1, ms(time.Since(start)), float64(status)
	if status < 200 || status >= 400 {
		obj.Status, obj.Output = agent.Critical, fmt.Sprintf("status %d", status)
		m["up"] = 0
		return []agent.Object{obj}, nil
	}
	if n.metricsURL != "" {
		if n.metricsURL != probe {
			body, status, err = n.fetch(ctx, n.metricsURL)
			if err != nil || status != 200 {
				obj.Status, obj.Output = agent.Warning, "metrics_url did not answer"
				return []agent.Object{obj}, nil
			}
		}
		n.prom(&obj, parseProm(string(body)))
	}
	return []agent.Object{obj}, nil
}

func (n *node) fetch(ctx context.Context, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return b, resp.StatusCode, nil
}

// parseProm sums every sample of a metric name, ignoring labels and
// comments: enough for the process and nodejs_* defaults.
func parseProm(s string) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || l[0] == '#' {
			continue
		}
		i := strings.IndexAny(l, "{ ")
		if i < 0 {
			continue
		}
		name := l[:i]
		j := strings.LastIndexByte(l, ' ')
		v, err := strconv.ParseFloat(strings.TrimSpace(l[j+1:]), 64)
		if err != nil {
			continue
		}
		out[name] += v
	}
	return out
}

func (n *node) prom(obj *agent.Object, p map[string]float64) {
	m := obj.Metrics
	if v, ok := p["nodejs_eventloop_lag_seconds"]; ok {
		m["node_event_loop_lag_ms"] = v * 1000
	}
	if v, ok := p["nodejs_heap_size_used_bytes"]; ok {
		m["node_heap_used_bytes"] = v
	}
	if v, ok := p["nodejs_active_handles_total"]; ok {
		m["node_active_handles"] = v
	} else if v, ok := p["nodejs_active_handles"]; ok {
		m["node_active_handles"] = v
	}
	if v, ok := p["process_resident_memory_bytes"]; ok {
		m["mem_used_bytes"] = v
	}
	if v, ok := p["process_start_time_seconds"]; ok {
		m["uptime_seconds"] = max(float64(n.now().Unix())-v, 0)
	}
	if v, ok := p["process_cpu_seconds_total"]; ok {
		if r, ok := n.cpu.next(n.now(), v); ok {
			m["cpu_usage_pct"] = r * 100 // one core fully busy = 100
		}
	}
}
