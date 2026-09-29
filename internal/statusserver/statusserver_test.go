package statusserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func newTestServer(t *testing.T, opts Options) http.Handler {
	t.Helper()
	if opts.Gatherer == nil {
		opts.Gatherer = prometheus.NewRegistry()
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.AddReadiness("database", func(context.Context) error { return errors.New("unreachable") })
	return s.Handler()
}

func get(h http.Handler, path, token, remote string, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuth(t *testing.T) {
	h := newTestServer(t, Options{Token: "t0ken", Status: func() any { return map[string]string{"node": "n1"} }})

	if rec := get(h, "/healthz", "", "", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 without token", rec.Code)
	}
	if rec := get(h, "/readyz", "", "", ""); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "unreachable") {
		t.Errorf("/readyz = %d %q, want 503 naming the failed check", rec.Code, rec.Body)
	}
	for _, path := range []string{"/status", "/metrics"} {
		if rec := get(h, path, "", "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, rec.Code)
		}
		if rec := get(h, path, "wrong", "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with wrong token = %d, want 401", path, rec.Code)
		}
		if rec := get(h, path, "t0ken", "", ""); rec.Code != http.StatusOK {
			t.Errorf("%s with token = %d, want 200", path, rec.Code)
		}
	}
	if rec := get(h, "/status", "t0ken", "", "text/html"); !strings.Contains(rec.Body.String(), "<pre>") {
		t.Errorf("/status for a browser should be HTML, got %q", rec.Body)
	}
}

func TestAllowedNetworks(t *testing.T) {
	h := newTestServer(t, Options{AllowedNetworks: []string{"10.0.0.0/8"}})
	if rec := get(h, "/healthz", "", "10.1.2.3:5000", ""); rec.Code != http.StatusOK {
		t.Errorf("allowed source = %d, want 200", rec.Code)
	}
	if rec := get(h, "/healthz", "", "192.168.1.1:5000", ""); rec.Code != http.StatusForbidden {
		t.Errorf("other source = %d, want 403", rec.Code)
	}
}
