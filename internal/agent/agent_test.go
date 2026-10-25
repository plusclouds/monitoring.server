package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type fake struct {
	objs []Object
	err  error
}

func (f fake) Collect(context.Context) ([]Object, error) { return f.objs, f.err }

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadConfigEnvOnly(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("pt_abc\n"), 0o600)
	c, err := LoadConfig("", env(map[string]string{
		"AGENT_URL": "https://m.example/ingest/v1/x", "AGENT_TOKEN_FILE": tok, "AGENT_ID": "web-1",
		"AGENT_INTERVAL": "10s", "AGENT_COLLECTORS": "host, redis",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "pt_abc" || c.AgentID != "web-1" || c.Interval != 10*time.Second || len(c.Collectors) != 2 {
		t.Errorf("config: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	c.Interval = time.Second
	if c.Validate() == nil {
		t.Error("1s interval accepted")
	}
}

func TestLoadConfigFileStrict(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.yaml")
	_ = os.WriteFile(p, []byte("url: http://x\ncollectors:\n  - type: tcp\n    name: db\n    addr: 127.0.0.1:5432\n"), 0o600)
	c, err := LoadConfig(p, env(nil))
	if err != nil || c.Collectors[0].Options["addr"] != "127.0.0.1:5432" {
		t.Fatalf("%+v %v", c, err)
	}
	_ = os.WriteFile(p, []byte("urll: x\n"), 0o600)
	if _, err := LoadConfig(p, env(nil)); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestOptionsEnvFallback(t *testing.T) {
	o := NewOptions("redis", "", map[string]any{"db": 2}, env(map[string]string{"AGENT_REDIS_ADDR": "r:6379"}))
	if o.String("addr", "") != "r:6379" {
		t.Error("env not used")
	}
	if n, _ := o.Int("db", 0); n != 2 {
		t.Error("file value lost")
	}
	if o.Name != "redis" {
		t.Error("name default")
	}
	sec := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(sec, []byte("hunter2\n"), 0o600)
	if got := NewOptions("redis", "", map[string]any{"password_file": sec}, env(nil)).String("password", ""); got != "hunter2" {
		t.Errorf("password_file: %q", got)
	}
	if got := NewOptions("redis", "", nil, env(map[string]string{"AGENT_REDIS_PASSWORD_FILE": sec})).String("password", ""); got != "hunter2" {
		t.Errorf("env _FILE: %q", got)
	}
}

func TestCollectErrorBecomesUnknownObject(t *testing.T) {
	Register("t-fake", func(Options) (Collector, error) { return fake{err: errors.New("boom")}, nil })
	a, err := New(Config{AgentID: "a", Interval: 10 * time.Second, Collectors: []CollectorConfig{{Type: "t-fake"}}}, "v", discard)
	if err != nil {
		t.Fatal(err)
	}
	r := a.Collect(context.Background())
	if len(r.Objects) != 1 || r.Objects[0].Status != Unknown || r.Objects[0].Key != "collector:t-fake" {
		t.Errorf("objects: %+v", r.Objects)
	}
	if _, err := New(Config{Collectors: []CollectorConfig{{Type: "nope"}}}, "v", discard); err == nil {
		t.Error("unknown collector accepted")
	}
}

func TestPushRetriesThenDelivers(t *testing.T) {
	var calls atomic.Int32
	var auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	p, _ := NewPusher(Config{URL: srv.URL, Token: "tok"})
	p.sleep = func(context.Context, time.Duration) {}
	if err := p.Send(context.Background(), Report{V: 1, AgentID: "a"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || auth != "Bearer tok" || !strings.Contains(body, `"agent_id":"a"`) {
		t.Errorf("calls=%d auth=%q body=%q", calls.Load(), auth, body)
	}
}

func TestPushRefusalIsFinal(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	p, _ := NewPusher(Config{URL: srv.URL, Token: "x"})
	p.sleep = func(context.Context, time.Duration) {}
	err := p.Send(context.Background(), Report{})
	if err == nil || calls.Load() != 1 || p.Errors() != 1 || !strings.Contains(err.Error(), "401") {
		t.Errorf("err=%v calls=%d errors=%d", err, calls.Load(), p.Errors())
	}
}

func TestEncodeFitsBody(t *testing.T) {
	r := Report{V: 1, AgentID: "a"}
	for i := 0; i < 2000; i++ {
		r.Objects = append(r.Objects, Object{Key: "container:" + strings.Repeat("x", 40), Kind: "container",
			Metrics: map[string]float64{"container_running": 1, "container_restarts": 0}})
	}
	r.Objects[0].Key = "host"
	b, err := Encode(r)
	if err != nil || len(b) > MaxBody {
		t.Fatalf("len=%d err=%v", len(b), err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil || back.Objects[0].Key != "host" || !strings.Contains(back.Objects[0].Output, "left out") {
		t.Errorf("host object: %+v %v", back.Objects[0], err)
	}
	if r.Objects[0].Output != "" {
		t.Error("caller's object was changed")
	}
}
