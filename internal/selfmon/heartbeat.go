package selfmon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/buildinfo"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/liveness"
	"github.com/plusclouds/monitoring.server/internal/tlsconf"
	"github.com/plusclouds/monitoring.server/internal/webhook"
)

// Heartbeat is the dead man's switch (F11): a signed monitoring.heartbeat
// event to each configured receiver, sent only while the core loop is
// healthy, so a process that is up but stuck goes silent too. It runs as
// a job of the maintenance node holding the maintenance lock.
type Heartbeat struct {
	System  *pgxpool.Pool
	NodeID  string
	Targets []Target
	// SelfCheck requires the engine to have applied its own monitor.self
	// result recently (self_monitoring.store_as_metrics).
	SelfCheck bool
	Logger    *slog.Logger
	Client    *http.Client

	sent *prometheus.CounterVec
}

// Target is one receiver with its token and signing secret, read from
// their files.
type Target struct {
	URL    string
	Token  string // Authorization: Bearer
	Secret string // Standard Webhooks signing secret (whsec_...)
}

// NewHeartbeat reads the targets' secrets and registers the counter.
func NewHeartbeat(c config.SelfMonitoring, p config.TLSPolicy, db *pgxpool.Pool, node string, log *slog.Logger,
	reg prometheus.Registerer) (*Heartbeat, error) {
	h := &Heartbeat{System: db, NodeID: node, SelfCheck: c.StoreAsMetrics, Logger: log,
		sent: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "selfmon_heartbeats_total",
			Help: "Heartbeats by outcome: sent, failed, withheld (core loop unhealthy)."}, []string{"outcome"})}
	for i, t := range c.Heartbeat.Targets {
		if t.URL == "" { // unset: no such target
			continue
		}
		key := fmt.Sprintf("self_monitoring.heartbeat.targets[%d]", i)
		token, err := config.ReadSecret(key+".token", "", t.TokenFile)
		if err != nil {
			return nil, err
		}
		secret, err := config.ReadSecret(key+".secret", "", t.SecretFile)
		if err != nil {
			return nil, err
		}
		h.Targets = append(h.Targets, Target{URL: t.URL, Token: token, Secret: secret})
	}
	base, err := tlsconf.Base(p)
	if err != nil {
		return nil, err
	}
	h.Client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: base,
		Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}}
	if reg != nil {
		if err := reg.Register(h.sent); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// Healthy says why the heartbeat must be withheld, or nil: the database
// answers, this process's loops turn, and the engine applied its own
// result within 3 minutes.
func (h *Heartbeat) Healthy(ctx context.Context) (time.Time, error) {
	if err := liveness.Check(2 * time.Minute); err != nil {
		return time.Time{}, err
	}
	var last *time.Time
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.System.QueryRow(qctx, `
		SELECT max(cs.last_result_at) FROM check_state cs
		  JOIN checks c ON c.id = cs.check_id JOIN tenants t ON t.id = c.tenant_id
		 WHERE t.is_platform AND c.plugin = 'monitor.self'`).Scan(&last); err != nil {
		return time.Time{}, fmt.Errorf("database: %w", err)
	}
	if !h.SelfCheck {
		if last == nil {
			return time.Time{}, nil
		}
		return *last, nil
	}
	if last == nil || time.Since(*last) > 3*time.Minute {
		return time.Time{}, errors.New("the engine has not applied its own health result in the last 3 minutes")
	}
	return *last, nil
}

// Send runs once: withheld when unhealthy, otherwise posted to every
// target. A failing target is logged and counted; the others still get it.
func (h *Heartbeat) Send(ctx context.Context) error {
	if len(h.Targets) == 0 {
		return nil
	}
	last, err := h.Healthy(ctx)
	if err != nil {
		h.sent.WithLabelValues("withheld").Inc()
		h.log().Warn("heartbeat withheld", "reason", err.Error())
		return nil
	}
	now := time.Now().UTC()
	id := uuid.Must(uuid.NewV7()).String()
	data := map[string]any{"node_id": h.NodeID, "version": buildinfo.Version}
	if !last.IsZero() {
		data["last_self_result_at"] = last.UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(map[string]any{"id": id, "type": "monitoring.heartbeat",
		"timestamp": now.Format(time.RFC3339), "data": data})
	if err != nil {
		return err
	}
	for _, t := range h.Targets {
		if err := h.post(ctx, t, id, now, body); err != nil {
			h.sent.WithLabelValues("failed").Inc()
			h.log().Warn("heartbeat not delivered", "url", redactURL(t.URL), "error", err)
			continue
		}
		h.sent.WithLabelValues("sent").Inc()
	}
	return nil
}

func (h *Heartbeat) post(ctx context.Context, t Target, id string, now time.Time, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "monitor-heartbeat/"+buildinfo.Version)
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(now.Unix(), 10))
	if t.Secret != "" {
		sig, err := webhook.Sign([]string{t.Secret}, id, now.Unix(), body)
		if err != nil {
			return err
		}
		req.Header.Set("webhook-signature", sig)
	}
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	res, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	return nil
}

func (h *Heartbeat) log() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// redactURL keeps the host and hides the path, which often holds the
// receiver's secret (hc-ping.com/<uuid>).
func redactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "(invalid URL)"
	}
	return p.Scheme + "://" + p.Host + "/…"
}
