package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

func opts(typ string, m map[string]any) agent.Options {
	return agent.NewOptions(typ, "t", m, func(string) (string, bool) { return "", false })
}

func one(t *testing.T, c agent.Collector) agent.Object {
	t.Helper()
	objs, err := c.Collect(context.Background())
	if err != nil || len(objs) != 1 {
		t.Fatalf("%v %v", objs, err)
	}
	return objs[0]
}

func TestTCP(t *testing.T) {
	l, _ := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	c, err := newTCP(opts("tcp", map[string]any{"addr": l.Addr().String()}))
	if err != nil {
		t.Fatal(err)
	}
	if o := one(t, c); o.Metrics["up"] != 1 || o.Status != "" {
		t.Errorf("open port: %+v", o)
	}
	_ = l.Close()
	if o := one(t, c); o.Metrics["up"] != 0 || o.Status != agent.Critical {
		t.Errorf("closed port: %+v", o)
	}
	if _, err := newTCP(opts("tcp", map[string]any{"addr": "nope"})); err == nil {
		t.Error("bad addr accepted")
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			http.Error(w, "x", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	get := func(path string, extra map[string]any) agent.Object {
		m := map[string]any{"url": srv.URL + path}
		for k, v := range extra {
			m[k] = v
		}
		c, err := newHTTP(opts("http", m))
		if err != nil {
			t.Fatal(err)
		}
		return one(t, c)
	}
	if o := get("/ok", map[string]any{"contains": "healthy"}); o.Metrics["up"] != 1 || o.Metrics["http_status"] != 200 {
		t.Errorf("ok: %+v", o)
	}
	if o := get("/ok", map[string]any{"contains": "degraded"}); o.Status != agent.Critical {
		t.Errorf("body check: %+v", o)
	}
	if o := get("/bad", nil); o.Status != agent.Critical || o.Metrics["http_status"] != 500 {
		t.Errorf("500: %+v", o)
	}
	if o := get("/bad", map[string]any{"status": []any{500}}); o.Status != "" {
		t.Errorf("accepted 500: %+v", o)
	}
	if o := get("/redirect", nil); o.Metrics["http_status"] != 302 || o.Status != "" {
		t.Errorf("redirect is reported, not followed: %+v", o)
	}
	srv.Close()
	if o := get("/ok", nil); o.Status != agent.Critical || o.Metrics["up"] != 0 {
		t.Errorf("down: %+v", o)
	}
}

func TestProcess(t *testing.T) {
	root := t.TempDir()
	mk := func(pid, comm, cmdline string) {
		d := filepath.Join(root, pid)
		_ = os.MkdirAll(d, 0o750)
		_ = os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o600)
		_ = os.WriteFile(filepath.Join(d, "cmdline"), []byte(cmdline), 0o600)
	}
	mk("1", "nginx", "nginx\x00-g\x00daemon off;")
	mk("7", "nginx", "nginx: worker process")
	mk("9", "sh", "sh\x00-c\x00echo")
	_ = os.MkdirAll(filepath.Join(root, "net"), 0o750) // a non-PID entry
	c, err := newProcess(opts("process", map[string]any{"match": "nginx", "proc": root}))
	if err != nil {
		t.Fatal(err)
	}
	if o := one(t, c); o.Metrics["process_count"] != 2 || o.Status != "" {
		t.Errorf("running: %+v", o)
	}
	c, _ = newProcess(opts("process", map[string]any{"match": "postgres", "proc": root}))
	if o := one(t, c); o.Status != agent.Critical || o.Metrics["up"] != 0 {
		t.Errorf("absent: %+v", o)
	}
	if _, err := newProcess(opts("process", nil)); err == nil {
		t.Error("empty match accepted")
	}
}
