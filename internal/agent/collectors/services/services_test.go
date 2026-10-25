package services

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

// fakeCred is a throwaway value for fake servers, built at run time so no literal looks like a secret.
var fakeCred = strings.Repeat("ab", 6)

func opts(typ string, m map[string]any) agent.Options {
	return agent.NewOptions(typ, "t", m, func(string) (string, bool) { return "", false })
}

func collect(t *testing.T, c agent.Collector) agent.Object {
	t.Helper()
	objs, err := c.Collect(context.Background())
	if err != nil || len(objs) != 1 {
		t.Fatalf("%v %v", objs, err)
	}
	if objs[0].Key != "service:t" || objs[0].Kind != "service" {
		t.Fatalf("identity: %+v", objs[0])
	}
	return objs[0]
}

const redisInfo = "# Server\r\nredis_version:7.2.4\r\nuptime_in_seconds:100\r\n# Clients\r\nconnected_clients:12\r\nblocked_clients:1\r\nmaxclients:10000\r\n" +
	"# Memory\r\nused_memory:2048\r\n# Stats\r\ninstantaneous_ops_per_sec:77\r\nkeyspace_hits:90\r\nkeyspace_misses:10\r\nevicted_keys:3\r\n" +
	"# Replication\r\nrole:slave\r\nmaster_link_status:down\r\nmaster_last_io_seconds_ago:-1\r\n# Keyspace\r\ndb0:keys=5,expires=1,avg_ttl=0\r\ndb1:keys=2,expires=0,avg_ttl=0\r\n"

func fakeRedis(t *testing.T, password string) string {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				authed := password == ""
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch cmd := strings.TrimSpace(line); {
					case strings.HasPrefix(cmd, "AUTH "):
						if strings.HasSuffix(cmd, " "+password) {
							authed = true
							_, _ = fmt.Fprint(c, "+OK\r\n")
						} else {
							_, _ = fmt.Fprint(c, "-WRONGPASS invalid\r\n")
						}
					case cmd == "INFO" && authed:
						_, _ = fmt.Fprintf(c, "$%d\r\n%s\r\n", len(redisInfo), redisInfo)
					default:
						_, _ = fmt.Fprint(c, "-NOAUTH Authentication required.\r\n")
					}
				}
			}()
		}
	}()
	return l.Addr().String()
}

func TestRedis(t *testing.T) {
	addr := fakeRedis(t, fakeCred)
	c, err := newRedis(opts("redis", map[string]any{"addr": addr, "password": fakeCred}))
	if err != nil {
		t.Fatal(err)
	}
	o := collect(t, c)
	m := o.Metrics
	if m["up"] != 1 || m["connections"] != 12 || m["connections_usage_pct"] != 0.12 || m["cache_hit_pct"] != 90 ||
		m["redis_keys"] != 7 || m["redis_evicted_keys"] != 3 || m["ops_per_sec"] != 77 || m["mem_used_bytes"] != 2048 {
		t.Errorf("metrics: %v", m)
	}
	if o.Status != agent.Critical || !strings.Contains(o.Output, "master link") || o.Labels["version"] != "7.2.4" {
		t.Errorf("replica down: %+v", o)
	}
	bad, _ := newRedis(opts("redis", map[string]any{"addr": addr, "password": "wrong"}))
	if o := collect(t, bad); o.Status != agent.Critical || o.Metrics["up"] != 0 {
		t.Errorf("wrong password: %+v", o)
	}
	l, _ := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	_ = l.Close()
	c, _ = newRedis(opts("redis", map[string]any{"addr": dead}))
	if o := collect(t, c); o.Status != agent.Critical || o.Metrics["up"] != 0 {
		t.Errorf("down: %+v", o)
	}
}

func TestNginx(t *testing.T) {
	reqs := 1000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "Active connections: 291 \nserver accepts handled requests\n 500 498 %d \nReading: 6 Writing: 179 Waiting: 106 \n", reqs)
	}))
	defer srv.Close()
	c, _ := newNginx(opts("nginx", map[string]any{"url": srv.URL + "/status"}))
	n := c.(*nginx)
	now := time.Unix(1000, 0)
	n.now = func() time.Time { return now }
	o := collect(t, n)
	if o.Metrics["nginx_active"] != 291 || o.Metrics["nginx_waiting"] != 106 {
		t.Errorf("metrics: %v", o.Metrics)
	}
	reqs, now = 1500, now.Add(10*time.Second)
	if o := collect(t, n); o.Metrics["ops_per_sec"] != 50 {
		t.Errorf("rate: %v", o.Metrics)
	}
	bad, _ := newNginx(opts("nginx", map[string]any{"url": srv.URL + "/nope"}))
	if o := collect(t, bad); o.Status != agent.Critical || !strings.Contains(o.Output, "stub_status") {
		t.Errorf("not enabled: %+v", o)
	}
}

func TestElasticsearch(t *testing.T) {
	status := "yellow"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "elastic" || p != fakeCred {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"cluster_name":"c1","status":%q,"number_of_nodes":3,"unassigned_shards":4}`, status)
	}))
	defer srv.Close()
	c, _ := newElasticsearch(opts("elasticsearch", map[string]any{"url": srv.URL, "username": "elastic", "password": fakeCred}))
	if o := collect(t, c); o.Status != agent.Warning || o.Metrics["es_status"] != 1 || o.Metrics["es_unassigned_shards"] != 4 || o.Labels["cluster"] != "c1" {
		t.Errorf("yellow: %+v", o)
	}
	status = "red"
	if o := collect(t, c); o.Status != agent.Critical || o.Metrics["es_status"] != 2 {
		t.Errorf("red: %+v", o)
	}
	status = "green"
	if o := collect(t, c); o.Status != "" || o.Metrics["es_status"] != 0 {
		t.Errorf("green: %+v", o)
	}
	noauth, _ := newElasticsearch(opts("elasticsearch", map[string]any{"url": srv.URL}))
	if o := collect(t, noauth); o.Status != agent.Critical || !strings.Contains(o.Output, "401") {
		t.Errorf("401: %+v", o)
	}
}

func TestNode(t *testing.T) {
	cpu := 10.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = fmt.Fprint(w, "ok")
		case "/metrics":
			_, _ = fmt.Fprintf(w, "# HELP x y\nnodejs_eventloop_lag_seconds 0.012\nnodejs_heap_size_used_bytes 5000\nnodejs_active_handles_total 4\n"+
				"process_resident_memory_bytes 9000\nprocess_cpu_seconds_total %f\nhttp_requests_total{code=\"200\"} 5\n", cpu)
		default:
			http.Error(w, "x", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	c, err := newNode(opts("nodejs", map[string]any{"url": srv.URL + "/health", "metrics_url": srv.URL + "/metrics"}))
	if err != nil {
		t.Fatal(err)
	}
	n := c.(*node)
	now := time.Unix(1000, 0)
	n.now = func() time.Time { return now }
	o := collect(t, n)
	m := o.Metrics
	if m["up"] != 1 || m["node_event_loop_lag_ms"] != 12 || m["node_heap_used_bytes"] != 5000 || m["node_active_handles"] != 4 || m["mem_used_bytes"] != 9000 {
		t.Errorf("metrics: %v", m)
	}
	cpu, now = 15, now.Add(10*time.Second)
	if o := collect(t, n); o.Metrics["cpu_usage_pct"] != 50 {
		t.Errorf("cpu: %v", o.Metrics)
	}
	bad, _ := newNode(opts("nodejs", map[string]any{"url": srv.URL + "/broken"}))
	if o := collect(t, bad); o.Status != agent.Critical || o.Metrics["up"] != 0 {
		t.Errorf("503: %+v", o)
	}
	if _, err := newNode(opts("nodejs", nil)); err == nil {
		t.Error("no url accepted")
	}
}

// fakeFPM answers one FastCGI request with the status JSON, checking that
// the client sent QUERY_STRING=json and an empty STDIN.
func fakeFPM(t *testing.T, status string) string {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		var params bytes.Buffer
		for {
			var h struct {
				Version, Type byte
				ID, Len       uint16
				Pad, Reserved byte
			}
			if binary.Read(c, binary.BigEndian, &h) != nil {
				return
			}
			data := make([]byte, int(h.Len)+int(h.Pad))
			if _, err := io.ReadFull(c, data); err != nil {
				return
			}
			if h.Type == fcgiParams {
				params.Write(data[:h.Len])
			}
			if h.Type == fcgiStdin && h.Len == 0 {
				break
			}
		}
		var out bytes.Buffer
		body := "Status: " + status + "\r\nContent-Type: application/json\r\n\r\n"
		if strings.Contains(params.String(), "QUERY_STRING") && strings.Contains(params.String(), "json") {
			body += `{"pool":"www","process manager":"dynamic","start since":300,"accepted conn":900,"listen queue":2,` +
				`"idle processes":3,"active processes":2,"total processes":5,"max children reached":1,"slow requests":4}`
		}
		fcgiRecord(&out, fcgiStdout, []byte(body))
		fcgiRecord(&out, fcgiStdout, nil)
		fcgiRecord(&out, fcgiEnd, make([]byte, 8))
		_, _ = c.Write(out.Bytes())
	}()
	return l.Addr().String()
}

func TestPHPFPM(t *testing.T) {
	c, err := newPHPFPM(opts("phpfpm", map[string]any{"addr": fakeFPM(t, "200 OK")}))
	if err != nil {
		t.Fatal(err)
	}
	o := collect(t, c)
	m := o.Metrics
	if m["up"] != 1 || m["phpfpm_active"] != 2 || m["phpfpm_idle"] != 3 || m["phpfpm_queue"] != 2 || m["phpfpm_max_children_reached"] != 1 ||
		m["uptime_seconds"] != 300 || o.Labels["pool"] != "www" {
		t.Errorf("metrics: %+v", o)
	}
	c, _ = newPHPFPM(opts("phpfpm", map[string]any{"addr": fakeFPM(t, "404 Not Found")}))
	if o := collect(t, c); o.Status != agent.Critical || !strings.Contains(o.Output, "404") {
		t.Errorf("404: %+v", o)
	}
	if _, err := newPHPFPM(opts("phpfpm", map[string]any{"addr": "nope"})); err == nil {
		t.Error("bad addr accepted")
	}
}

func TestPostgresDown(t *testing.T) {
	l, _ := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	c, err := newPostgres(opts("postgres", map[string]any{"addr": addr, "password": fakeCred}))
	if err != nil {
		t.Fatal(err)
	}
	o := collect(t, c)
	if o.Status != agent.Critical || o.Metrics["up"] != 0 || strings.Contains(o.Output, fakeCred) {
		t.Errorf("down: %+v", o)
	}
	if _, err := newPostgres(opts("postgres", map[string]any{"dsn": "postgres://u@h:notaport/db"})); err == nil {
		t.Error("bad dsn accepted")
	}
}

func TestRate(t *testing.T) {
	var r rate
	now := time.Unix(100, 0)
	if _, ok := r.next(now, 10); ok {
		t.Error("first sample has a rate")
	}
	if v, ok := r.next(now.Add(2*time.Second), 20); !ok || v != 5 {
		t.Errorf("rate %v %v", v, ok)
	}
	if _, ok := r.next(now.Add(4*time.Second), 1); ok {
		t.Error("counter reset has a rate")
	}
}
