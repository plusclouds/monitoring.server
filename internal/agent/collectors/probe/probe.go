// Package probe holds the local liveness probes: tcp (a port accepts
// connections), http (an endpoint answers) and process (a process with this
// name runs). They run from inside the container, so they see what the
// service sees: localhost, the container's own network.
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

func init() {
	agent.Register("tcp", newTCP)
	agent.Register("http", newHTTP)
	agent.Register("process", newProcess)
}

// ---- tcp ----

type tcp struct {
	name, addr string
	timeout    time.Duration
}

func newTCP(o agent.Options) (agent.Collector, error) {
	addr := o.String("addr", "")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("addr %q must be host:port", addr)
	}
	to, err := o.Duration("timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &tcp{o.Name, addr, to}, nil
}

func (t *tcp) Collect(ctx context.Context) ([]agent.Object, error) {
	obj := agent.Object{Key: "tcp:" + t.name, Kind: "tcp", Name: t.name, Labels: map[string]string{"addr": t.addr}}
	start := time.Now()
	d := net.Dialer{Timeout: t.timeout}
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		obj.Status, obj.Output = agent.Critical, shortErr(err)
		obj.Metrics = map[string]float64{"up": 0}
		return []agent.Object{obj}, nil
	}
	_ = conn.Close()
	obj.Metrics = map[string]float64{"up": 1, "response_ms": ms(time.Since(start))}
	return []agent.Object{obj}, nil
}

// ---- http ----

type httpProbe struct {
	name, url, method string
	want              []int // accepted status codes; empty = 2xx and 3xx
	contains          string
	client            *http.Client
	header            map[string]string
}

func newHTTP(o agent.Options) (agent.Collector, error) {
	url := o.String("url", "")
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("url %q must start with http:// or https://", url)
	}
	to, err := o.Duration("timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	p := &httpProbe{name: o.Name, url: url, method: strings.ToUpper(o.String("method", "GET")), contains: o.String("contains", ""),
		header: map[string]string{}}
	for _, s := range o.List("status", nil) {
		n, err := strconv.Atoi(s)
		if err != nil || n < 100 || n > 599 {
			return nil, fmt.Errorf("status %q is not an HTTP status code", s)
		}
		p.want = append(p.want, n)
	}
	if tok := o.String("bearer_token", ""); tok != "" {
		p.header["Authorization"] = "Bearer " + tok
	}
	insecure := o.String("insecure_skip_verify", "") == "true"
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec // opt-in, for self-signed local endpoints
	// A health endpoint that redirects is reported as it answers.
	p.client = &http.Client{Timeout: to, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return p, nil
}

func (p *httpProbe) accepted(code int) bool {
	if len(p.want) == 0 {
		return code >= 200 && code < 400
	}
	for _, w := range p.want {
		if w == code {
			return true
		}
	}
	return false
}

func (p *httpProbe) Collect(ctx context.Context) ([]agent.Object, error) {
	obj := agent.Object{Key: "http:" + p.name, Kind: "http", Name: p.name, Labels: map[string]string{"url": p.url}}
	req, err := http.NewRequestWithContext(ctx, p.method, p.url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range p.header {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		obj.Status, obj.Output = agent.Critical, shortErr(err)
		obj.Metrics = map[string]float64{"up": 0}
		return []agent.Object{obj}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	elapsed := time.Since(start)
	obj.Metrics = map[string]float64{"up": 1, "http_status": float64(resp.StatusCode), "response_ms": ms(elapsed)}
	switch {
	case !p.accepted(resp.StatusCode):
		obj.Status, obj.Output = agent.Critical, fmt.Sprintf("status %d", resp.StatusCode)
		obj.Metrics["up"] = 0
	case p.contains != "" && !strings.Contains(string(body), p.contains):
		obj.Status, obj.Output = agent.Critical, fmt.Sprintf("body does not contain %q", p.contains)
		obj.Metrics["up"] = 0
	}
	return []agent.Object{obj}, nil
}

// ---- process ----

type process struct {
	name, match, proc string
}

func newProcess(o agent.Options) (agent.Collector, error) {
	m := o.String("match", "")
	if m == "" {
		return nil, errors.New("match is required: part of the process name or command line")
	}
	return &process{o.Name, m, o.String("proc", "/proc")}, nil
}

// Collect counts the processes whose name or command line contains match,
// reading /proc, so it needs no tool in the image. In a container without
// a shared PID namespace it sees the container's own processes.
func (p *process) Collect(context.Context) ([]agent.Object, error) {
	ents, err := os.ReadDir(p.proc)
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		if p.matches(filepath.Join(p.proc, e.Name())) {
			n++
		}
	}
	obj := agent.Object{Key: "process:" + p.name, Kind: "process", Name: p.name, Labels: map[string]string{"match": p.match},
		Metrics: map[string]float64{"process_count": float64(n), "up": 1}}
	if n == 0 {
		obj.Status, obj.Output = agent.Critical, "no process matches"
		obj.Metrics["up"] = 0
	}
	return []agent.Object{obj}, nil
}

func (p *process) matches(dir string) bool {
	// dir is a /proc/<pid> entry.
	if b, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil && strings.Contains(strings.TrimSpace(string(b)), p.match) { //nolint:gosec // see above
		return true
	}
	b, err := os.ReadFile(filepath.Join(dir, "cmdline")) //nolint:gosec // see above
	return err == nil && strings.Contains(strings.ReplaceAll(string(b), "\x00", " "), p.match)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// shortErr trims a dial or HTTP error to its cause.
func shortErr(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
