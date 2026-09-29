package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/auth"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

// withRequestID accepts a caller's X-Request-ID when it is short and
// printable, otherwise generates one, and echoes it in the response.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = uuid.Must(uuid.NewV7()).String()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func validRequestID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// withClientIP resolves the client address. X-Forwarded-For is only trusted
// when the direct peer is a trusted proxy; the right-most address that is not
// a trusted proxy is the client.
func withClientIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var ip netip.Addr
			if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
				ip = ap.Addr().Unmap()
			}
			if ip.IsValid() && isTrusted(ip) {
				hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
				for i := len(hops) - 1; i >= 0; i-- {
					a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
					if err != nil {
						break
					}
					ip = a.Unmap()
					if !isTrusted(ip) {
						break
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey, ip)))
		})
	}
}

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func withAccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			attrs := []any{
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(), "request_id", requestID(r.Context()),
				"client_ip", clientIP(r.Context()).String(),
			}
			if p := principal(r.Context()); p != nil {
				attrs = append(attrs, "api_key_id", p.KeyID)
				if p.Tenant != nil {
					attrs = append(attrs, "tenant_id", p.Tenant.ID)
				}
			}
			log.InfoContext(r.Context(), "request", attrs...)
		})
	}
}

func withRecover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(v)
					}
					log.ErrorContext(r.Context(), "panic in handler", "panic", fmt.Sprint(v), "stack", string(debug.Stack()),
						"request_id", requestID(r.Context()))
					writeProblem(w, r, log, errors.New("panic"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func withMaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// keyRow is what auth_lookup_key returns.
type keyRow struct {
	ID          uuid.UUID
	TenantID    *uuid.UUID
	Kind        string
	Hash        []byte
	Role        string
	ExpiresAt   *time.Time
	IPAllowlist []netip.Prefix
	RevokedAt   *time.Time
}

// authenticate resolves the Principal (F01 "Resolving the caller").
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		p, err := s.resolve(ctx, r)
		if err != nil {
			writeProblem(w, r, s.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, p)))
	})
}

func (s *Server) resolve(ctx context.Context, r *http.Request) (*Principal, error) {
	ip := clientIP(ctx)
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || presented == "" {
		return nil, s.failAuth(ctx, "missing", "")
	}
	prefix, err := auth.ParseKey(presented)
	if err != nil {
		return nil, s.failAuth(ctx, "malformed", "")
	}
	var k keyRow
	err = s.db.QueryRow(ctx, `SELECT id, tenant_id, kind, hash, role, expires_at, ip_allowlist, revoked_at
	                            FROM auth_lookup_key($1)`, prefix).
		Scan(&k.ID, &k.TenantID, &k.Kind, &k.Hash, &k.Role, &k.ExpiresAt, &k.IPAllowlist, &k.RevokedAt)
	if err != nil {
		return nil, s.failAuth(ctx, "unknown", prefix)
	}
	switch {
	case !auth.MatchKey(presented, k.Hash):
		return nil, s.failAuth(ctx, "mismatch", prefix)
	case k.RevokedAt != nil:
		return nil, s.failAuth(ctx, "revoked", prefix)
	case k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt):
		return nil, s.failAuth(ctx, "expired", prefix)
	case len(k.IPAllowlist) > 0 && !inPrefixes(ip, k.IPAllowlist):
		return nil, s.failAuth(ctx, "source-not-allowed", prefix)
	}
	s.touch(ctx, k.ID)

	p := &Principal{KeyID: k.ID, Role: k.Role, Platform: k.Kind == "platform"}
	p.Actor = audit.Actor{Kind: audit.ActorAPIKey, APIKeyID: &p.KeyID, RequestID: requestID(ctx), SourceIP: ip}

	if !p.Platform {
		t, err := tenancy.ByID(ctx, s.db, *k.TenantID)
		if err != nil || t.Status == tenancy.StatusDeleted {
			return nil, s.failAuth(ctx, "tenant-deleted", prefix)
		}
		p.Tenant = &t
		return p, nil
	}

	p.Actor.Kind = audit.ActorPlatform
	if err := s.resolvePlatformTenant(ctx, r, p); err != nil {
		return nil, err
	}
	return p, nil
}

// resolvePlatformTenant applies X-Tenant-External-ID / X-Tenant-ID and
// X-Actor-External-ID for the platform key (ADR-0012).
func (s *Server) resolvePlatformTenant(ctx context.Context, r *http.Request, p *Principal) error {
	extTenant := r.Header.Get("X-Tenant-External-ID")
	tenantID := r.Header.Get("X-Tenant-ID")
	var t tenancy.Tenant
	var err error
	switch {
	case extTenant != "" && tenantID != "":
		return problem(http.StatusBadRequest, "tenant-headers", "Conflicting tenant headers",
			"Send X-Tenant-External-ID or X-Tenant-ID, not both.")
	case extTenant != "":
		t, err = s.tenants.EnsureTenant(ctx, p.Actor, s.identitySource, extTenant)
	case tenantID != "":
		id, perr := uuid.Parse(tenantID)
		if perr != nil {
			return problem(http.StatusBadRequest, "invalid-header", "Invalid X-Tenant-ID", "X-Tenant-ID must be a UUID.")
		}
		t, err = tenancy.ByID(ctx, s.db, id)
	default:
		if r.Header.Get("X-Actor-External-ID") != "" {
			return errTenantRequired
		}
		return nil // provisioning endpoints need no tenant
	}
	if err != nil {
		return err
	}
	if t.Status == tenancy.StatusDeleted {
		return errNotFound
	}
	p.Tenant = &t

	extActor := r.Header.Get("X-Actor-External-ID")
	if extActor == "" {
		return nil // the platform itself, with full rights in the tenant
	}
	u, role, err := s.tenants.EnsureActor(ctx, p.Actor, t.ID, s.identitySource, extActor)
	switch {
	case errors.Is(err, tenancy.ErrNotFound):
		return errNotMember
	case err != nil:
		return err
	case u.Status != "active":
		return problem(http.StatusForbidden, "user-disabled", "User disabled", "The acting user is disabled.")
	}
	p.User, p.Role = &u, role
	p.Actor.Kind, p.Actor.UserID, p.Actor.ExternalID = audit.ActorUser, &u.ID, extActor
	return nil
}

// failAuth records a failed authentication (F01) and returns 401. Records
// are limited per source address so a flood cannot fill the audit log; the
// metric counts every failure.
func (s *Server) failAuth(ctx context.Context, reason, prefix string) error {
	s.authFailures.WithLabelValues(reason).Inc()
	ip := clientIP(ctx)
	if s.failLimiter(ip).Allow() {
		platform, err := tenancy.PlatformID(ctx, s.db)
		if err == nil {
			actor := audit.Actor{Kind: audit.ActorSystem, Detail: "unauthenticated", RequestID: requestID(ctx), SourceIP: ip}
			err = s.writeAudit(ctx, actor.Event(platform, "auth.failed", "api_key", prefix, nil, map[string]any{"reason": reason}))
		}
		if err != nil {
			s.log.WarnContext(ctx, "cannot record failed authentication", "error", err)
		}
	}
	return errUnauthenticated
}

func (s *Server) failLimiter(ip netip.Addr) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.failLimits[ip]
	if !ok {
		if len(s.failLimits) > 10000 {
			clear(s.failLimits) // bound memory; losing buckets only allows a few extra records
		}
		l = rate.NewLimiter(rate.Every(time.Second), 60)
		s.failLimits[ip] = l
	}
	return l
}

// touch records key use at most once a minute per key and node.
func (s *Server) touch(ctx context.Context, id uuid.UUID) {
	s.mu.Lock()
	last := s.touched[id]
	now := time.Now()
	if now.Sub(last) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.touched[id] = now
	s.mu.Unlock()
	if _, err := s.db.Exec(ctx, `SELECT touch_api_key($1)`, id); err != nil {
		s.log.WarnContext(ctx, "cannot record key use", "error", err)
	}
}

// rateLimit applies a token bucket per key (ADR-0007): the tenant's
// api_rate_per_minute, or the platform rate. 0 means unlimited. Limits are
// per API node, which is enough to protect the service.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r.Context())
		perMinute := s.platformRate
		if !p.Platform {
			perMinute = p.Tenant.Limits.APIRatePerMinute
		}
		if perMinute > 0 && !s.keyLimiter(p.KeyID, perMinute).Allow() {
			s.rateLimited.Inc()
			w.Header().Set("Retry-After", "1")
			writeProblem(w, r, s.log, errRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) keyLimiter(id uuid.UUID, perMinute int) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := rate.Limit(float64(perMinute) / 60)
	burst := max(perMinute/6, 1) // ten seconds' worth
	l, ok := s.keyLimits[id]
	if !ok {
		l = rate.NewLimiter(limit, burst)
		s.keyLimits[id] = l
	} else if l.Limit() != limit {
		l.SetLimit(limit)
		l.SetBurst(burst)
	}
	return l
}

func inPrefixes(a netip.Addr, list []netip.Prefix) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
