// Package statusserver is the node status server of the core (F11) and of
// probes (F09): /healthz, /readyz, /status and /metrics on their own listener.
//
// /healthz and /readyz are open, for load balancers and container runtimes.
// /status and /metrics need the status token when one is configured.
package statusserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/tlsconf"
)

// Options configure a Server.
type Options struct {
	Listen          string
	TLS             config.ListenerTLS
	Policy          config.TLSPolicy
	Token           string   // empty = no authentication (loopback only, enforced by config validation)
	AllowedNetworks []string // empty = any source
	Gatherer        prometheus.Gatherer
	Status          func() any // body of /status
	Logger          *slog.Logger
}

// Server serves the status endpoints.
type Server struct {
	opts    Options
	allowed []netip.Prefix

	mu     sync.RWMutex
	checks []readiness
}

type readiness struct {
	name  string
	check func(context.Context) error
}

func New(opts Options) (*Server, error) {
	s := &Server{opts: opts}
	for _, n := range opts.AllowedNetworks {
		p, err := netip.ParsePrefix(n)
		if err != nil {
			return nil, err
		}
		s.allowed = append(s.allowed, p)
	}
	return s, nil
}

// AddReadiness registers a check that /readyz runs. Roles add theirs as they
// start (database reachable, shards owned, notifier connected).
func (s *Server) AddReadiness(name string, check func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, readiness{name, check})
}

// Handler returns the HTTP handler, for tests and for embedding.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /status", s.auth(http.HandlerFunc(s.status)))
	if s.opts.Gatherer != nil {
		mux.Handle("GET /metrics", s.auth(promhttp.HandlerFor(s.opts.Gatherer, promhttp.HandlerOpts{})))
	}
	return s.sourceFilter(mux)
}

// Run serves until ctx is done, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.opts.Logger.Handler(), slog.LevelWarn),
	}
	if !s.opts.TLS.Disabled {
		tc, err := tlsconf.Server(s.opts.Policy, s.opts.TLS)
		if err != nil {
			return err
		}
		srv.TLSConfig = tc
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", s.opts.Listen)
	if err != nil {
		return err
	}
	s.opts.Logger.Info("status server listening", "addr", ln.Addr().String(), "tls", !s.opts.TLS.Disabled)

	errc := make(chan error, 1)
	go func() {
		if srv.TLSConfig != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	checks := append([]readiness(nil), s.checks...)
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	result := map[string]string{}
	ready := true
	for _, c := range checks {
		if err := c.check(ctx); err != nil {
			result[c.name] = err.Error()
			ready = false
		} else {
			result[c.name] = "ok"
		}
	}
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"ready": ready, "checks": result})
}

var statusPage = template.Must(template.New("status").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>monitor status</title>
<style>body{font:14px/1.4 system-ui,sans-serif;margin:16px}pre{background:#f4f4f4;padding:12px;overflow:auto}</style>
</head><body><h1>Node status</h1><pre>{{.}}</pre></body></html>`))

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var body any = map[string]any{}
	if s.opts.Status != nil {
		body = s.opts.Status()
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		b, err := json.MarshalIndent(body, "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = statusPage.Execute(w, string(b))
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.opts.Token == "" {
		return next
	}
	want := []byte("Bearer " + s.opts.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="monitor-status"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sourceFilter(next http.Handler) http.Handler {
	if len(s.allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err == nil {
			ip := ap.Addr().Unmap()
			for _, p := range s.allowed {
				if p.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
