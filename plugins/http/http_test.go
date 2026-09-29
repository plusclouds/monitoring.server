package http

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/pkg/plugin/plugintest"
)

func server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("Welcome home")) })
	mux.HandleFunc("/fail", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusInternalServerError) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) { time.Sleep(2 * time.Second) })
	mux.HandleFunc("/private", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "monitor" || p != "s3cret-pass" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("secret area"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestHTTPCheck(t *testing.T) {
	ts := server(t)
	c := &Check{}
	cases := []struct {
		name   string
		cfg    Config
		creds  map[string]plugin.Credential
		status plugin.Status
		output string
	}{
		{"ok with keyword", Config{URL: ts.URL + "/ok", Keyword: "Welcome"}, nil, plugin.OK, "200 OK"},
		{"keyword missing", Config{URL: ts.URL + "/ok", Keyword: "Goodbye"}, nil, plugin.Critical, `keyword "Goodbye" not found`},
		{"keyword present", Config{URL: ts.URL + "/ok", KeywordAbsent: "Welcome"}, nil, plugin.Critical, `keyword "Welcome" present`},
		{"server error", Config{URL: ts.URL + "/fail"}, nil, plugin.Critical, "unexpected status 500"},
		{"expected 500", Config{URL: ts.URL + "/fail", ExpectedStatus: []int{500}}, nil, plugin.OK, "500"},
		{"redirect followed", Config{URL: ts.URL + "/redirect", Keyword: "Welcome"}, nil, plugin.OK, "200 OK"},
		{"redirect not followed", Config{URL: ts.URL + "/redirect", FollowRedirects: new(false)}, nil, plugin.OK, "302"},
		{"basic auth", Config{URL: ts.URL + "/private", Keyword: "secret area"}, map[string]plugin.Credential{
			"auth": {Type: "http_basic", Fields: map[string]string{"username": "monitor"}, Secret: map[string]plugin.Secret{"password": "s3cret-pass"}},
		}, plugin.OK, "200 OK"},
		{"missing auth", Config{URL: ts.URL + "/private"}, nil, plugin.Critical, "401"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := plugintest.Run(t, c, plugintest.Options{Config: tc.cfg, Credentials: tc.creds})
			if r.Status != tc.status || !strings.Contains(r.Output, tc.output) {
				t.Fatalf("got %v %q, want %v containing %q", r.Status, r.Output, tc.status, tc.output)
			}
			if r.Metrics[mStatus] < 200 || r.Metrics[mTotal] <= 0 || r.Metrics[mConnect] < 0 {
				t.Errorf("metrics not filled: %v", r.Metrics)
			}
		})
	}
}

// F02: test connection reports "timeout" and "connection refused" as
// distinct errors.
func TestHTTPErrorsAreDistinct(t *testing.T) {
	ts := server(t)
	r := plugintest.Run(t, &Check{}, plugintest.Options{Config: Config{URL: ts.URL + "/slow"}, Timeout: 300 * time.Millisecond})
	if r.Status != plugin.Critical || r.Output != "timeout" {
		t.Errorf("slow server: got %v %q", r.Status, r.Output)
	}

	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	r = plugintest.Run(t, &Check{}, plugintest.Options{Address: "http://" + addr + "/"})
	if r.Status != plugin.Critical || r.Output != "connection refused" {
		t.Errorf("closed port: got %v %q", r.Status, r.Output)
	}
}

// The SSRF guard refuses private targets for customer tenants, reported as
// UNKNOWN because it is a configuration problem.
func TestHTTPNetworkPolicy(t *testing.T) {
	ts := server(t)
	customer := &plugin.NetPolicy{DenyPrivate: true}
	r := plugintest.Run(t, &Check{}, plugintest.Options{Config: Config{URL: ts.URL + "/ok"}, Network: customer})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "private or local address") {
		t.Errorf("got %v %q", r.Status, r.Output)
	}
	allowLoopback := &plugin.NetPolicy{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	r = plugintest.Run(t, &Check{}, plugintest.Options{Config: Config{URL: ts.URL + "/ok"}, Network: allowLoopback})
	if r.Status != plugin.OK {
		t.Errorf("allowed network: got %v %q", r.Status, r.Output)
	}
}

func TestHTTPValidate(t *testing.T) {
	c := &Check{}
	for _, bad := range []string{
		`{"url":"ftp://x"}`, `{"expected_status":[99]}`, `{"source_ip":"nope"}`, `{"unknown":1}`,
	} {
		if err := c.Validate([]byte(bad)); err == nil {
			t.Errorf("Validate(%s) should fail", bad)
		}
	}
}
