package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

const id1 = "aaaaaaaaaaaa1111"
const id2 = "bbbbbbbbbbbb2222"
const id3 = "cccccccccccc3333"

// fakeEngine serves just enough of the Docker API over a unix socket.
func fakeEngine(t *testing.T, cpu *float64) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"Id":"%s","Names":["/web"],"Image":"nginx","State":"running","Labels":{"com.docker.compose.project":"shop"}},
			{"Id":"%s","Names":["/db"],"Image":"postgres","State":"exited","Labels":{}},
			{"Id":"%s","Names":["/skip"],"Image":"x","State":"running","Labels":{"monitor.ignore":"true"}}]`, id1, id2, id3)
	})
	mux.HandleFunc("/containers/"+id1+"/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"RestartCount":2,"State":{"Status":"running","Running":true,"StartedAt":"2026-10-25T10:00:00Z","Health":{"Status":"unhealthy"}}}`)
	})
	mux.HandleFunc("/containers/"+id2+"/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"RestartCount":0,"State":{"Status":"exited","Running":false,"ExitCode":137,"OOMKilled":true}}`)
	})
	mux.HandleFunc("/containers/"+id1+"/stats", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":%f},"system_cpu_usage":%f,"online_cpus":2},
			"memory_stats":{"usage":3000,"limit":10000,"stats":{"inactive_file":1000}},
			"networks":{"eth0":{"rx_bytes":%f,"tx_bytes":0}}}`, *cpu, *cpu*10, *cpu*100)
	})
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "d.sock")
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

func TestDocker(t *testing.T) {
	cpu := 1000.0
	sock := fakeEngine(t, &cpu)
	c, err := New(sock, "monitor.enable=true") // a label selects stopped containers too
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	objs, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]agent.Object{}
	for _, o := range objs {
		by[o.Key] = o
	}
	if len(objs) != 3 || by["docker:engine"].Metrics["up"] != 1 || by["container:skip"].Key != "" {
		t.Fatalf("objects: %+v", objs)
	}
	web, db := by["container:web"], by["container:db"]
	if web.Status != agent.Critical || web.Metrics["container_restarts"] != 2 || web.Metrics["container_healthy"] != 0 ||
		web.Metrics["mem_used_bytes"] != 2000 || web.Metrics["mem_usage_pct"] != 20 || web.Metrics["uptime_seconds"] != 2*3600 ||
		web.Labels["project"] != "shop" {
		t.Errorf("web: %+v", web)
	}
	if _, ok := web.Metrics["cpu_usage_pct"]; ok {
		t.Error("cpu reported on the first sample")
	}
	if db.Status != agent.Critical || db.Metrics["container_exit_code"] != 137 || db.Metrics["container_running"] != 0 {
		t.Errorf("db: %+v", db)
	}

	// 10 s later the container used 100 ticks of 1000 system ticks on 2 CPUs: 20%.
	cpu, now = 1100, now.Add(10*time.Second)
	objs, _ = c.Collect(context.Background())
	for _, o := range objs {
		if o.Key == "container:web" && (o.Metrics["cpu_usage_pct"] < 19.99 || o.Metrics["cpu_usage_pct"] > 20.01 || o.Metrics["net_rx_bytes_per_sec"] != 1000) {
			t.Errorf("rates: %+v", o.Metrics)
		}
	}
}

// Without a label, a container that was stopped before the agent started is
// left out; one the agent saw running stays reported when it stops.
func TestDockerStoppedWithoutLabel(t *testing.T) {
	cpu := 1.0
	c, _ := New(fakeEngine(t, &cpu), "")
	objs, _ := c.Collect(context.Background())
	for _, o := range objs {
		if o.Key == "container:db" {
			t.Errorf("old stopped container reported: %+v", o)
		}
	}
	c.seen[id2] = true // as if it had been running
	objs, _ = c.Collect(context.Background())
	found := false
	for _, o := range objs {
		found = found || (o.Key == "container:db" && o.Status == agent.Critical)
	}
	if !found {
		t.Errorf("crashed container not reported: %+v", objs)
	}
}

func TestDockerEngineDown(t *testing.T) {
	c, _ := New(filepath.Join(t.TempDir(), "none.sock"), "")
	objs, err := c.Collect(context.Background())
	if err != nil || len(objs) != 1 || objs[0].Status != agent.Critical || objs[0].Metrics["up"] != 0 {
		t.Errorf("%+v %v", objs, err)
	}
}
