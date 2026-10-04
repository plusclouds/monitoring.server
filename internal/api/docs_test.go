package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/plusclouds/monitoring.server/internal/config"
)

func docsHandler(t *testing.T, enabled bool) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.API.Docs = enabled
	s, err := New(Options{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func getPath(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	return rec
}

func TestDocsEndpoints(t *testing.T) {
	h := docsHandler(t, true)

	page := getPath(h, "/docs")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `<redoc spec-url="/v1/openapi.json"`) {
		t.Fatalf("/docs = %d %q", page.Code, page.Body.String())
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "http") {
		t.Errorf("/docs CSP should allow only this server's scripts: %q", csp)
	}

	js := getPath(h, "/docs/redoc.standalone.js")
	if js.Code != http.StatusOK || js.Body.Len() < 1_000_000 || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("redoc asset = %d, %d bytes, %s", js.Code, js.Body.Len(), js.Header().Get("Content-Type"))
	}

	spec := getPath(h, "/v1/openapi.json")
	var doc map[string]any
	if spec.Code != http.StatusOK || json.Unmarshal(spec.Body.Bytes(), &doc) != nil || doc["openapi"] != "3.1.0" {
		t.Fatalf("/v1/openapi.json = %d, openapi %v", spec.Code, doc["openapi"])
	}
	if _, ok := doc["paths"].(map[string]any)["/plugins"]; !ok {
		t.Error("spec is missing /plugins")
	}

	yaml := getPath(h, "/v1/openapi.yaml")
	if yaml.Code != http.StatusOK || !strings.HasPrefix(yaml.Body.String(), "openapi: 3.1.0") {
		t.Errorf("/v1/openapi.yaml = %d", yaml.Code)
	}
}

func TestDocsCanBeDisabled(t *testing.T) {
	h := docsHandler(t, false)
	if rec := getPath(h, "/docs"); rec.Code != http.StatusNotFound {
		t.Errorf("/docs = %d, want 404 when api.docs is false", rec.Code)
	}
	// With docs off, the spec path falls through to the authenticated API
	// (401 on a real server; this test has no database behind it), so the
	// spec is not served without a key.
	if rec := getPath(h, "/v1/openapi.json"); rec.Code == http.StatusOK {
		t.Errorf("/v1/openapi.json served without a key while api.docs is false")
	}
}
