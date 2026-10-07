package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/ingest"
	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/runner"
)

// M5 (F08): a device pushes JSON for a push.http check; the values become
// metrics and state, thresholds alert, silence goes CRITICAL after
// missed_count intervals, and a rotated token stops the old one.
func TestHTTPPush(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	dev := x.device("cold-room")
	created := x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "sensor", "plugin": "push.http",
		"config": map[string]any{"metrics": map[string]any{"temperature": "$.t", "humidity": "$.h"},
			"units": map[string]any{"temperature": "celsius"}, "timestamp": "$.ts"},
		"thresholds": []any{map[string]any{"metric": "temperature", "critical": map[string]any{"op": ">", "value": 8}}}}), 201)
	chk := x.id(created)
	token, _ := created.body["push_token"].(string)
	push, _ := created.body["push"].(map[string]any)
	if !strings.HasPrefix(token, "mpush_") || push == nil || push["ingest_path"] != "/ingest/v1/"+chk || created.body["failure_count"] != float64(1) ||
		!strings.HasPrefix(token, push["token_prefix"].(string)) {
		t.Fatalf("created: %s", created.raw)
	}
	if got := x.must(x.do("GET", "/v1/checks/"+chk, x.key, nil), 200); got.body["push_token"] != nil || got.body["push"] == nil {
		t.Errorf("the token is shown again: %s", got.raw)
	}

	results := make(chan runner.Result, 100)
	in, err := ingest.New(ingest.Options{Config: config.IngestHTTP{MaxBody: 1024, RateLimit: config.Rate{PerSecond: 1, Burst: 20}},
		System: db.System, Results: results, Logger: slog.New(slog.DiscardHandler), Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(in.Handler())
	defer srv.Close()
	w, err := metrics.NewWriter(metrics.WriterOptions{System: db.System})
	if err != nil {
		t.Fatal(err)
	}
	post := func(id, tok, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/ingest/v1/"+id, strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	// apply hands what the listener queued to the engine and the metrics store.
	apply := func() int {
		t.Helper()
		n := 0
		for {
			select {
			case r := <-results:
				if _, err := x.eng.Apply(ctx, r); err != nil {
					t.Fatal(err)
				}
				w.Add(r)
				n++
			default:
				if err := w.Flush(ctx); err != nil {
					t.Fatal(err)
				}
				return n
			}
		}
	}
	state := func() map[string]any {
		return x.must(x.do("GET", "/v1/checks/"+chk+"/state", x.key, nil), 200).body
	}

	if code, _ := post(chk, "", `{"t":4}`); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := post(chk, "mpush_nope", `{"t":4}`); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code, body := post(chk, token, `{"t":"4.5","h":61}`); code != 202 || !strings.Contains(body, `"accepted":1`) {
		t.Fatalf("push: %d %s", code, body)
	}
	apply()
	st := state()
	if st["phase"] != "OK" || st["last_metrics"].(map[string]any)["temperature"] != 4.5 {
		t.Fatalf("state after a push: %v", st)
	}
	var names []string
	rows, _ := db.System.Query(ctx, `SELECT s.name || ':' || s.unit FROM metric_series s JOIN metric_groups g ON g.id = s.group_id
		WHERE g.source_id = $1 ORDER BY s.name`, chk)
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	if strings.Join(names, ",") != "humidity:,temperature:celsius" {
		t.Errorf("series: %v", names)
	}
	if p := x.must(x.do("GET", "/v1/checks/"+chk, x.key, nil), 200).body["push"].(map[string]any); p["last_push_at"] == nil {
		t.Errorf("last push not recorded: %v", p)
	}

	// The room warms up: the threshold opens an incident. A batch is applied oldest first.
	now := time.Now().Unix()
	code, body := post(chk, token, `[{"t":9,"ts":`+itoa(now)+`},{"t":7,"ts":`+itoa(now-30)+`}]`)
	if code != 202 || !strings.Contains(body, `"accepted":2`) {
		t.Fatalf("batch: %d %s", code, body)
	}
	if n := apply(); n != 2 {
		t.Errorf("batch applied %d", n)
	}
	if inc := x.incident(chk); inc["severity"] != "critical" {
		t.Errorf("threshold incident: %v", inc)
	}

	for body, want := range map[string]int{`nope`: 400, `{"t":1,"ts":"yesterday"}`: 400, strings.Repeat(" ", 2000): 413} {
		if code, _ := post(chk, token, body); code != want {
			t.Errorf("%.20q: %d, want %d", body, code, want)
		}
	}

	// Silence: after 3 intervals without data the sweep reports CRITICAL, once per interval.
	if _, err := db.System.Exec(ctx, `UPDATE push_sources SET last_push_at = now() - interval '10 minutes',
		created_at = now() - interval '1 hour' WHERE check_id = $1`, chk); err != nil {
		t.Fatal(err)
	}
	if n, err := in.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if n, _ := in.Sweep(ctx); n != 0 {
		t.Errorf("reported twice in one interval: %d", n)
	}
	apply()
	if st := state(); !strings.HasPrefix(st["last_output"].(string), "no data since") || st["phase"] != "PROBLEM" {
		t.Errorf("silent: %v", st)
	}
	// Data again: back to OK.
	if code, _ := post(chk, token, `{"t":3,"ts":`+itoa(time.Now().Unix())+`}`); code != 202 {
		t.Fatal(code)
	}
	apply()
	if st := state(); st["phase"] != "OK" {
		t.Errorf("after data returned: %v", st)
	}
	if n, _ := in.Sweep(ctx); n != 0 {
		t.Errorf("fresh check reported: %d", n)
	}

	// Rotation: the old token stops at once.
	rot := x.must(x.do("POST", "/v1/checks/"+chk+"/rotate-token", x.key, nil), 200)
	fresh, _ := rot.body["push_token"].(string)
	if fresh == "" || fresh == token {
		t.Fatalf("rotate: %s", rot.raw)
	}
	if code, _ := post(chk, token, `{"t":3,"ts":1}`); code != 401 {
		t.Errorf("old token after rotation: %d", code)
	}
	if code, _ := post(chk, fresh, `{"t":3,"ts":`+itoa(time.Now().Unix())+`}`); code != 202 {
		t.Errorf("new token: %d", code)
	}
	apply()

	// A disabled check refuses data; a polled check has no token.
	x.must(x.do("PATCH", "/v1/checks/"+chk, x.key, map[string]any{"enabled": false}), 200)
	if code, _ := post(chk, fresh, `{"t":3}`); code != 409 {
		t.Errorf("disabled: %d", code)
	}
	poll := x.check(dev, "ping", "icmp", false)
	if p := x.must(x.do("POST", "/v1/checks/"+poll+"/rotate-token", x.key, nil), 409); !strings.Contains(p.raw, "not-push") {
		t.Errorf("rotate a polled check: %s", p.raw)
	}
	if code, _ := post(poll, fresh, `{"t":3}`); code != 401 {
		t.Errorf("push to a polled check: %d", code)
	}

	// Config errors: unknown metric in a threshold, no metrics, Whoopsy! on a push check.
	x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "bad", "plugin": "push.http",
		"config":     map[string]any{"metrics": map[string]any{"t": "$.t"}},
		"thresholds": []any{map[string]any{"metric": "temperature", "critical": map[string]any{"op": ">", "value": 8}}}}), 422)
	x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "bad", "plugin": "push.http",
		"config": map[string]any{}}), 422)
	x.must(x.do("PUT", "/v1/checks/"+chk+"/whoopsy", x.key, map[string]any{"metric": "temperature"}), 422)

	// Rate limit per check.
	limited, err := ingest.New(ingest.Options{Config: config.IngestHTTP{MaxBody: 1024, RateLimit: config.Rate{PerSecond: 0.01, Burst: 1}},
		System: db.System, Results: results, Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	ls := httptest.NewServer(limited.Handler())
	defer ls.Close()
	codes := []int{}
	for range 2 {
		req, _ := http.NewRequestWithContext(ctx, "POST", ls.URL+"/ingest/v1/"+chk, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+fresh)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		codes = append(codes, res.StatusCode)
	}
	if codes[1] != 429 {
		t.Errorf("rate limit: %v", codes)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
