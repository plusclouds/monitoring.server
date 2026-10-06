package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	specfile "github.com/plusclouds/monitoring.server/api"
	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/buildinfo"
)

// Redoc 2.5.4 (MIT), vendored so the docs work without internet access.
// License: docs/redoc.LICENSE.txt.
//
//go:embed docs/redoc.standalone.js
var redocJS []byte

const docsPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Monitoring engine API</title>
<style>body{margin:0}</style>
</head>
<body>
<redoc spec-url="/v1/openapi.json" path-in-middle-panel="true" required-props-first="true"></redoc>
<script src="/docs/redoc.standalone.js"></script>
</body>
</html>
`

// docsCSP allows only this server's own script; Redoc needs inline styles
// and a blob worker for search.
const docsCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// versionLine is info.version in the YAML spec.
var versionLine = regexp.MustCompile(`(?m)^  version: .*$`)

// docsRoutes serves the API reference and the spec. They need no key: the
// spec is public, and it describes nothing about any tenant.
func (s *Server) docsRoutes(mux *http.ServeMux) error {
	spec, err := gen.GetSpec()
	if err != nil {
		return err
	}
	// The spec reports the running release, so clients see which server
	// version they talk to (the file's version is only a placeholder).
	specYAML := specfile.Spec
	if v := strings.TrimPrefix(buildinfo.Version, "v"); v != "dev" && v != "" {
		if loc := versionLine.FindIndex(specYAML); loc != nil {
			specYAML = slices.Concat(specYAML[:loc[0]], []byte("  version: "+v), specYAML[loc[1]:])
		}
		spec.Info.Version = v
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	started := time.Now()
	serve := func(contentType string, body []byte, cache string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", cache)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeContent(w, r, "", started, bytes.NewReader(body))
		}
	}
	mux.HandleFunc("GET /v1/openapi.json", serve("application/json", specJSON, "no-cache"))
	mux.HandleFunc("GET /v1/openapi.yaml", serve("application/yaml", specYAML, "no-cache"))
	mux.HandleFunc("GET /docs/redoc.standalone.js", serve("text/javascript; charset=utf-8", redocJS, "public, max-age=86400"))
	page := serve("text/html; charset=utf-8", []byte(docsPage), "no-cache")
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", docsCSP)
		w.Header().Set("Referrer-Policy", "no-referrer")
		page(w, r)
	})
	return nil
}
