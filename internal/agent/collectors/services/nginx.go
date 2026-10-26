package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

type nginx struct {
	name, url       string
	client          *http.Client
	requests, drops rate
	now             func() time.Time
}

func newNginx(o agent.Options) (agent.Collector, error) {
	to, err := o.Duration("timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	u := o.String("url", "http://127.0.0.1/nginx_status")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return nil, fmt.Errorf("url %q must start with http:// or https://", u)
	}
	return &nginx{name: o.Name, url: u, client: httpClient(to, o.String("insecure_skip_verify", "") == "true"), now: time.Now}, nil
}

// Collect reads ngx_http_stub_status:
//
//	Active connections: 291
//	server accepts handled requests
//	 16630948 16630948 31070465
//	Reading: 6 Writing: 179 Waiting: 106
func (n *nginx) Collect(ctx context.Context) ([]agent.Object, error) {
	obj := object("nginx", n.name, map[string]string{"url": n.url})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.url, nil)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := n.client.Do(req)
	if err != nil {
		return down(obj, err), nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return down(obj, fmt.Errorf("stub_status answered %d: enable it with `stub_status;` in a location", resp.StatusCode)), nil
	}
	st, err := parseStub(string(body))
	if err != nil {
		return down(obj, err), nil
	}
	now := n.now()
	m := obj.Metrics
	m["up"], m["response_ms"] = 1, ms(time.Since(start))
	m["nginx_active"], m["connections"], m["nginx_waiting"] = st.active, st.active, st.waiting
	if r, ok := n.requests.next(now, st.requests); ok {
		m["ops_per_sec"] = r
	}
	if r, ok := n.drops.next(now, st.accepts-st.handled); ok {
		m["errors_per_sec"] = r // connections accepted but not handled (worker_connections hit)
	}
	return []agent.Object{obj}, nil
}

type stub struct{ active, accepts, handled, requests, reading, writing, waiting float64 }

func parseStub(s string) (stub, error) {
	var st stub
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "Active connections:") {
		return st, fmt.Errorf("not nginx stub_status output")
	}
	f := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	st.active = f(strings.TrimSpace(strings.TrimPrefix(lines[0], "Active connections:")))
	c := strings.Fields(lines[2])
	if len(c) != 3 {
		return st, fmt.Errorf("stub_status: bad counters line")
	}
	st.accepts, st.handled, st.requests = f(c[0]), f(c[1]), f(c[2])
	w := strings.Fields(lines[3]) // Reading: 6 Writing: 179 Waiting: 106
	if len(w) == 6 {
		st.reading, st.writing, st.waiting = f(w[1]), f(w[3]), f(w[5])
	}
	return st, nil
}
