// Package ingest is the ingest role's HTTP listener (F08): devices POST
// JSON for a push check with that check's token, and the check's plugin
// turns the body into results for the engine. It also runs the last-seen
// rule: a push check that hears nothing for missed_count intervals gets a
// CRITICAL result.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/tlsconf"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Time window for pushed timestamps (F08): older or newer messages get the
// server time and a note in the output.
const (
	maxPast   = 24 * time.Hour
	maxFuture = 5 * time.Minute
)

// Options configure the ingest listener.
type Options struct {
	Config   config.IngestHTTP
	TLS      config.TLSPolicy
	System   *pgxpool.Pool // BYPASSRLS: finds any tenant's push check by ID
	Results  chan<- runner.Result
	Logger   *slog.Logger
	Registry prometheus.Registerer
	// SweepEvery is how often the last-seen rule runs (default 5 s).
	SweepEvery time.Duration
}

// Server receives pushed data.
type Server struct {
	o   Options
	log *slog.Logger

	mu       sync.Mutex
	limiters map[uuid.UUID]*limiter

	messages *prometheus.CounterVec
	stale    prometheus.Counter
	statuses *prometheus.CounterVec
}

type limiter struct {
	*rate.Limiter
	seen time.Time
}

// New builds the listener and registers its metrics.
func New(o Options) (*Server, error) {
	if o.SweepEvery <= 0 {
		o.SweepEvery = 5 * time.Second
	}
	s := &Server{
		o: o, log: o.Logger, limiters: map[uuid.UUID]*limiter{},
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ingest_http_messages_total", Help: "Pushed HTTP messages by outcome.",
		}, []string{"outcome"}),
		statuses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ingest_http_responses_total", Help: "Answers of the push endpoint by HTTP status code.",
		}, []string{"code"}),
		stale: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ingest_stale_results_total", Help: "Last-seen results sent for silent push checks.",
		}),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if o.Registry != nil {
		for _, c := range []prometheus.Collector{s.messages, s.stale, s.statuses} {
			if err := o.Registry.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// Handler serves the ingest endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/v1/{check_id}", s.counted(s.push))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return mux
}

// Run serves until ctx ends and applies the last-seen rule meanwhile.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Handler:           http.TimeoutHandler(s.Handler(), 30*time.Second, `{"title":"Request timeout"}`),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	var err error
	if s.o.Config.TLS.CertFile != "" {
		if srv.TLSConfig, err = tlsconf.Server(s.o.TLS, s.o.Config.TLS); err != nil {
			return err
		}
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", s.o.Config.Listen)
	if err != nil {
		return err
	}
	s.log.Info("ingest listening", "addr", ln.Addr().String(), "tls", srv.TLSConfig != nil)
	errc := make(chan error, 1)
	go func() {
		if srv.TLSConfig != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	tick := time.NewTicker(s.o.SweepEvery)
	defer tick.Stop()
	for {
		select {
		case err := <-errc:
			return err
		case <-tick.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("last-seen sweep", "error", err)
			}
			s.forgetLimiters()
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("ingest shutdown: %w", err)
			}
			if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	}
}

// source is a push check as the listener needs it.
type source struct {
	tokenHash []byte
	tenant    uuid.UUID
	device    uuid.UUID
	plugin    string
	config    json.RawMessage
	interval  int
	enabled   bool
	tenantOK  bool
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("check_id"))
	if err != nil {
		s.fail(w, http.StatusNotFound, "not-found", "Not found", "No push check with this ID.")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ingest"`)
		s.fail(w, http.StatusUnauthorized, "unauthenticated", "Unauthenticated",
			"Send the check's ingest token as `Authorization: Bearer <token>`.")
		return
	}
	if !s.allow(id) {
		w.Header().Set("Retry-After", "1")
		s.fail(w, http.StatusTooManyRequests, "rate-limited", "Too many requests", "This check's message rate is exceeded.")
		return
	}
	var src source
	err = s.o.System.QueryRow(r.Context(), `
		SELECT ps.token_hash, c.tenant_id, c.device_id, c.plugin, c.config, c.interval_seconds, c.enabled,
		       t.status = 'active'
		  FROM push_sources ps
		  JOIN checks c ON c.id = ps.check_id
		  JOIN tenants t ON t.id = c.tenant_id
		 WHERE ps.check_id = $1`, id).
		Scan(&src.tokenHash, &src.tenant, &src.device, &src.plugin, &src.config, &src.interval, &src.enabled, &src.tenantOK)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !inventory.MatchPushToken(token, src.tokenHash)) {
		// The same answer for an unknown check and a wrong token.
		w.Header().Set("WWW-Authenticate", `Bearer realm="ingest", error="invalid_token"`)
		s.fail(w, http.StatusUnauthorized, "unauthenticated", "Unauthenticated", "The token is not valid for this check.")
		return
	}
	if err != nil {
		s.log.Error("push lookup", "check_id", id, "error", err)
		s.fail(w, http.StatusInternalServerError, "internal", "Internal error", "")
		return
	}
	switch {
	case !src.tenantOK:
		s.fail(w, http.StatusForbidden, "tenant-suspended", "Tenant suspended", "The tenant does not accept data now.")
		return
	case !src.enabled:
		s.fail(w, http.StatusConflict, "check-disabled", "Check disabled", "The check is disabled; the message was dropped.")
		return
	}
	p, ok := plugin.Lookup(src.plugin)
	in, isIngester := p.(plugin.Ingester)
	if !ok || !isIngester {
		s.fail(w, http.StatusConflict, "not-push", "Not a push check", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(s.o.Config.MaxBody)))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		s.fail(w, http.StatusRequestEntityTooLarge, "too-large", "Body too large",
			fmt.Sprintf("At most %d bytes.", tooBig.Limit))
		return
	}
	if err != nil {
		s.fail(w, http.StatusBadRequest, "bad-request", "Bad request", "The body could not be read.")
		return
	}
	results, err := in.Parse(src.config, body)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "invalid-message", "Invalid message", err.Error())
		return
	}
	layout, err := in.MetricsFor(src.config)
	if err != nil {
		s.fail(w, http.StatusConflict, "invalid-config", "Invalid check config", err.Error())
		return
	}
	now := time.Now().UTC()
	var newest time.Time
	interval := time.Duration(src.interval) * time.Second
	for _, res := range results {
		res.Time = Window(&res, now)
		if res.Time.After(newest) {
			newest = res.Time
		}
		rr := runner.Result{TenantID: src.tenant, DeviceID: src.device, CheckID: id, Plugin: src.plugin,
			Interval: interval, Scheduled: res.Time, Result: res, Layout: layout}
		select {
		case s.o.Results <- rr:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			s.fail(w, http.StatusServiceUnavailable, "busy", "Busy", "The engine is not keeping up; retry later.")
			return
		}
	}
	if _, err := s.o.System.Exec(r.Context(), `
		UPDATE push_sources SET last_push_at = greatest(last_push_at, $2), stale_reported_at = NULL
		 WHERE check_id = $1`, id, newest); err != nil {
		s.log.Error("push last seen", "check_id", id, "error", err)
	}
	s.messages.WithLabelValues("accepted").Add(float64(len(results)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintf(w, `{"accepted":%d}`+"\n", len(results))
}

// Window applies the timestamp rule (F08): no time is now; a time more than 24 h
// old or 5 minutes ahead is replaced by now, with a note.
func Window(r *plugin.Result, now time.Time) time.Time {
	if r.Time.IsZero() {
		return now
	}
	if r.Time.Before(now.Add(-maxPast)) || r.Time.After(now.Add(maxFuture)) {
		note := fmt.Sprintf(" (device time %s is off by %s; server time used)", r.Time.Format(time.RFC3339),
			r.Time.Sub(now).Round(time.Second))
		r.Output += note
		return now
	}
	return r.Time
}

func (s *Server) allow(id uuid.UUID) bool {
	rl := s.o.Config.RateLimit
	if rl.PerSecond <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.limiters[id]
	if l == nil {
		l = &limiter{Limiter: rate.NewLimiter(rate.Limit(rl.PerSecond), max(rl.Burst, 1))}
		s.limiters[id] = l
	}
	l.seen = time.Now()
	return l.Allow()
}

func (s *Server) forgetLimiters() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.limiters {
		if time.Since(l.seen) > 10*time.Minute {
			delete(s.limiters, id)
		}
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, typ, title, detail string) {
	s.messages.WithLabelValues(typ).Inc()
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "https://monitor.plusclouds.com/problems/" + typ, "title": title, "status": status, "detail": detail,
	})
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// counted counts the handler's answers by status code.
func (s *Server) counted(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		h(sw, r)
		if sw.code == 0 {
			sw.code = http.StatusOK
		}
		s.statuses.WithLabelValues(strconv.Itoa(sw.code)).Inc()
	}
}

// missedCount is a push check's missed_count (push.http and push.mqtt
// share the field), as the sweep query reads it.
func missedCount(cfg json.RawMessage) int {
	var c struct {
		MissedCount int `json:"missed_count"`
	}
	if json.Unmarshal(cfg, &c) != nil || c.MissedCount < 1 {
		return 3
	}
	return c.MissedCount
}

// Sweep sends a CRITICAL result for every enabled push check that has not
// heard from its device for missed_count intervals, at most once per
// interval while the silence lasts. A claim in the database makes it safe
// on several ingest nodes. It returns how many results it sent.
func (s *Server) Sweep(ctx context.Context) (int, error) {
	rows, err := s.o.System.Query(ctx, `
		UPDATE push_sources ps SET stale_reported_at = now()
		  FROM checks c, tenants t
		 WHERE c.id = ps.check_id AND t.id = c.tenant_id AND c.enabled AND t.status = 'active'
		   AND greatest(ps.last_push_at, ps.created_at) <
		       now() - make_interval(secs => c.interval_seconds * coalesce((c.config->>'missed_count')::int, 3))
		   AND (ps.stale_reported_at IS NULL OR ps.stale_reported_at < now() - make_interval(secs => c.interval_seconds))
		RETURNING ps.check_id, c.tenant_id, c.device_id, c.plugin, c.config, c.interval_seconds, ps.last_push_at`)
	if err != nil {
		return 0, err
	}
	type stale struct {
		id, tenant, device uuid.UUID
		plugin             string
		config             json.RawMessage
		interval           int
		last               *time.Time
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stale, error) {
		var x stale
		err := row.Scan(&x.id, &x.tenant, &x.device, &x.plugin, &x.config, &x.interval, &x.last)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	for _, x := range list {
		var layout []plugin.MetricDef
		if p, ok := plugin.Lookup(x.plugin); ok {
			layout = plugin.MetricsOf(p, x.config)
		}
		missed := missedCount(x.config)
		out := "no data received yet"
		if x.last != nil {
			out = "no data since " + x.last.UTC().Format(time.RFC3339) + " (" + strconv.Itoa(missed) + " intervals of " +
				(time.Duration(x.interval) * time.Second).String() + " missed)"
		}
		r := runner.Result{TenantID: x.tenant, DeviceID: x.device, CheckID: x.id, Plugin: x.plugin,
			Interval: time.Duration(x.interval) * time.Second, Scheduled: now, Layout: layout,
			Result: plugin.Result{Status: plugin.Critical, Output: out, Metrics: plugin.NaNs(len(layout)), Time: now}}
		select {
		case s.o.Results <- r:
			s.stale.Inc()
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return len(list), nil
}
