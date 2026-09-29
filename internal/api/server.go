// Package api is the REST API (ADR-0007). Handlers implement the interface
// generated from api/openapi.yaml; the spec also validates every request.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	middleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/store"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
	"github.com/plusclouds/monitoring.server/internal/tlsconf"
)

// Options configure the API server.
type Options struct {
	Config   config.Config
	DB       *pgxpool.Pool // the app role, subject to RLS
	Logger   *slog.Logger
	Registry prometheus.Registerer
}

// Server implements gen.StrictServerInterface.
type Server struct {
	cfg            config.API
	tls            config.TLSPolicy
	db             *pgxpool.Pool
	log            *slog.Logger
	tenants        *tenancy.Service
	identitySource string
	platformRate   int

	authFailures *prometheus.CounterVec
	rateLimited  prometheus.Counter

	mu         sync.Mutex
	keyLimits  map[uuid.UUID]*rate.Limiter
	failLimits map[netip.Addr]*rate.Limiter
	touched    map[uuid.UUID]time.Time
}

var _ gen.StrictServerInterface = (*Server)(nil)

// New builds the server and registers its metrics.
func New(o Options) (*Server, error) {
	s := &Server{
		cfg:            o.Config.API,
		tls:            o.Config.TLS,
		db:             o.DB,
		log:            o.Logger,
		identitySource: o.Config.Platform.IdentitySource,
		platformRate:   o.Config.API.PlatformRatePerMinute,
		tenants: &tenancy.Service{
			DB: o.DB, IdentitySource: o.Config.Platform.IdentitySource,
			Defaults: o.Config.Platform.TenantDefaults, JIT: o.Config.Platform.JIT,
		},
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_auth_failures_total", Help: "Failed API authentications by reason.",
		}, []string{"reason"}),
		rateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "api_rate_limited_total", Help: "Requests refused by per-key rate limits.",
		}),
		keyLimits:  map[uuid.UUID]*rate.Limiter{},
		failLimits: map[netip.Addr]*rate.Limiter{},
		touched:    map[uuid.UUID]time.Time{},
	}
	if o.Registry != nil {
		if err := errors.Join(o.Registry.Register(s.authFailures), o.Registry.Register(s.rateLimited)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Handler returns the full HTTP handler, mounted under /v1.
func (s *Server) Handler() (http.Handler, error) {
	spec, err := gen.GetSpec()
	if err != nil {
		return nil, err
	}
	spec.Servers = nil // routes are matched after /v1 is stripped
	validate := middleware.OapiRequestValidatorWithOptions(spec, &middleware.Options{
		Options: openapi3filter.Options{
			// Authentication is done by our own middleware before validation.
			AuthenticationFunc: func(context.Context, *openapi3filter.AuthenticationInput) error { return nil },
		},
		DoNotValidateServers: true,
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, r *http.Request, opts middleware.ErrorHandlerOpts) {
			status := opts.StatusCode
			if status == 0 {
				status = http.StatusBadRequest
			}
			writeProblem(w, r, s.log, problem(status, "invalid-request", http.StatusText(status), err.Error()))
		},
	})

	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, r, s.log, problem(http.StatusBadRequest, "invalid-request", "Invalid request", err.Error()))
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, r, s.log, err)
		},
	})
	routes := gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, r, s.log, problem(http.StatusBadRequest, "invalid-request", "Invalid request", err.Error()))
		},
	})

	trusted := make([]netip.Prefix, 0, len(s.cfg.TrustedProxies))
	for _, p := range s.cfg.TrustedProxies {
		pr, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, err
		}
		trusted = append(trusted, pr)
	}

	h := validate(routes)
	h = s.rateLimit(h)
	h = s.authenticate(h)
	h = withMaxBody(int64(s.cfg.MaxBody))(h)
	h = http.StripPrefix("/v1", h)
	mux := http.NewServeMux()
	mux.Handle("/v1/", h)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeProblem(w, r, s.log, errNotFound) })

	var out http.Handler = mux
	out = withAccessLog(s.log)(out)
	out = withRecover(s.log)(out)
	out = withClientIP(trusted)(out)
	out = withRequestID(out)
	return out, nil
}

// Run serves the API until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	h, err := s.Handler()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           http.TimeoutHandler(h, s.cfg.RequestTimeout.D(), `{"title":"Request timeout"}`),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	if s.cfg.TLS.CertFile != "" {
		if srv.TLSConfig, err = tlsconf.Server(s.tls, s.cfg.TLS); err != nil {
			return err
		}
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	s.log.Info("api listening", "addr", ln.Addr().String(), "tls", srv.TLSConfig != nil)

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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("api shutdown: %w", err)
		}
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// writeAudit writes one event in its own transaction scoped to its tenant.
func (s *Server) writeAudit(ctx context.Context, e audit.Event) error {
	return store.InTenant(ctx, s.db, e.TenantID, func(tx pgx.Tx) error { return audit.Write(ctx, tx, e) })
}
