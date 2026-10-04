package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/incident"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Options configure a notifier.
type Options struct {
	System   *pgxpool.Pool // BYPASSRLS
	Store    *Store
	Sender   *Sender
	Config   config.Notifier
	Deny     []netip.Prefix // outbound.deny_networks
	Logger   *slog.Logger
	Registry prometheus.Registerer
}

// Notifier routes events to endpoints and delivers them (ADR-0009). Several
// notifiers can run at once: they claim work with SKIP LOCKED.
type Notifier struct {
	o   Options
	log *slog.Logger

	deliveries *prometheus.CounterVec
	pending    prometheus.Gauge
	oldest     prometheus.Gauge
}

// New builds a notifier and registers its metrics.
func New(o Options) (*Notifier, error) {
	n := &Notifier{
		o: o, log: o.Logger,
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifier_deliveries_total", Help: "Webhook delivery attempts by result.",
		}, []string{"result"}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{Name: "notifier_outbox_depth", Help: "Pending deliveries."}),
		oldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "notifier_oldest_pending_seconds", Help: "Age of the oldest pending delivery.",
		}),
	}
	if o.Registry != nil {
		for _, c := range []prometheus.Collector{n.deliveries, n.pending, n.oldest} {
			if err := o.Registry.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return n, nil
}

// Run routes and delivers until ctx is done.
func (n *Notifier) Run(ctx context.Context) error {
	t := time.NewTicker(max(n.o.Config.PollInterval.D(), 100*time.Millisecond))
	defer t.Stop()
	for {
		n.Tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick routes waiting events and delivers due deliveries once.
func (n *Notifier) Tick(ctx context.Context) {
	for {
		k, err := n.route(ctx)
		if err != nil {
			if ctx.Err() == nil {
				n.log.Error("route events", "error", err)
			}
			break
		}
		if k == 0 {
			break
		}
	}
	for {
		k, err := n.dispatch(ctx)
		if err != nil {
			if ctx.Err() == nil {
				n.log.Error("deliver webhooks", "error", err)
			}
			break
		}
		if k == 0 {
			break
		}
	}
	var depth, age float64
	if err := n.o.System.QueryRow(ctx, `SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0)
		FROM webhook_deliveries WHERE status = 'pending'`).Scan(&depth, &age); err == nil {
		n.pending.Set(depth)
		n.oldest.Set(age)
	}
}

// routeInfo is data.route of a delivery.
type routeInfo struct {
	ID     uuid.UUID         `json:"id"`
	Name   string            `json:"name"`
	Step   int               `json:"step"`
	Labels map[string]string `json:"labels"`
}

// route turns a batch of unrouted events into deliveries.
func (n *Notifier) route(ctx context.Context) (int, error) {
	var count int
	err := pgx.BeginFunc(ctx, n.o.System, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, seq, type, subject, envelope FROM events
			WHERE routed_at IS NULL ORDER BY seq LIMIT $1 FOR UPDATE SKIP LOCKED`, max(n.o.Config.ClaimBatch, 1))
		if err != nil {
			return err
		}
		type ev struct {
			id, tenant uuid.UUID
			seq        int64
			typ        string
			subject    *string
			raw        []byte
		}
		evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ev, error) {
			var e ev
			err := r.Scan(&e.id, &e.tenant, &e.seq, &e.typ, &e.subject, &e.raw)
			return e, err
		})
		if err != nil || len(evs) == 0 {
			return err
		}
		routes := map[uuid.UUID][]Route{}
		enabled := map[uuid.UUID]bool{}
		ids := make([]uuid.UUID, 0, len(evs))
		for _, e := range evs {
			ids = append(ids, e.id)
			rs, ok := routes[e.tenant]
			if !ok {
				if rs, err = ListRoutes(ctx, tx, e.tenant); err != nil {
					return err
				}
				routes[e.tenant] = rs
			}
			var env incident.Envelope
			if err := json.Unmarshal(e.raw, &env); err != nil {
				n.log.Error("unreadable event", "event_id", e.id, "error", err)
				continue
			}
			severity := ""
			if obj, ok := env.Data.Object.(map[string]any); ok {
				severity, _ = obj["severity"].(string)
			}
			for _, r := range rs {
				if !r.Enabled || !r.Matches(&env, severity) {
					continue
				}
				on, ok := enabled[r.EndpointID]
				if !ok {
					if err := tx.QueryRow(ctx, `SELECT enabled FROM webhook_endpoints WHERE id = $1`, r.EndpointID).Scan(&on); err != nil {
						return err
					}
					enabled[r.EndpointID] = on
				}
				if on {
					info, _ := json.Marshal(routeInfo{ID: r.ID, Name: r.Name, Step: 0, Labels: nonNilMap(r.Labels)})
					id, err := uuid.NewV7()
					if err != nil {
						return err
					}
					subject := ""
					if e.subject != nil {
						subject = *e.subject
					}
					if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (id, tenant_id, event_id, event_seq, event_type,
						subject, endpoint_id, route_id, route) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
						ON CONFLICT (event_id, endpoint_id) DO NOTHING`,
						id, e.tenant, e.id, e.seq, e.typ, subject, r.EndpointID, r.ID, json.RawMessage(info)); err != nil {
						return err
					}
				}
				if !r.Continue {
					break
				}
			}
		}
		count = len(evs)
		_, err = tx.Exec(ctx, `UPDATE events SET routed_at = now() WHERE id = ANY($1)`, ids)
		return err
	})
	return count, err
}

// claimed is a delivery taken for sending.
type claimed struct {
	id, tenant, event, endpoint uuid.UUID
	route                       []byte
	attempts                    int
	created                     time.Time
}

// lease is how long a claimed delivery is reserved; a notifier that dies
// mid-send leaves it to be retried after this.
const lease = time.Minute

// dispatch sends a batch of due deliveries. A delivery waits while an
// earlier delivery of the same incident to the same endpoint is pending, so
// each receiver sees an incident's events in order (ADR-0009).
func (n *Notifier) dispatch(ctx context.Context) (int, error) {
	rows, err := n.o.System.Query(ctx, `
		UPDATE webhook_deliveries d SET next_attempt_at = now() + make_interval(secs => $2)
		 WHERE d.id IN (
			SELECT x.id FROM webhook_deliveries x
			 WHERE x.status = 'pending' AND x.next_attempt_at <= now()
			   AND NOT EXISTS (SELECT 1 FROM webhook_deliveries p
			                    WHERE p.endpoint_id = x.endpoint_id AND p.subject = x.subject
			                      AND p.status = 'pending' AND p.event_seq < x.event_seq)
			 ORDER BY x.event_seq LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING d.id, d.tenant_id, d.event_id, d.endpoint_id, d.route, d.attempts, d.created_at`,
		max(n.o.Config.ClaimBatch, 1), lease.Seconds())
	if err != nil {
		return 0, err
	}
	batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claimed, error) {
		var c claimed
		err := r.Scan(&c.id, &c.tenant, &c.event, &c.endpoint, &c.route, &c.attempts, &c.created)
		return c, err
	})
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	sem := make(chan struct{}, max(n.o.Config.Workers, 1))
	var wg sync.WaitGroup
	for _, c := range batch {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := n.deliver(ctx, c); err != nil && ctx.Err() == nil {
				n.log.Error("delivery", "delivery_id", c.id, "error", err)
			}
		})
	}
	wg.Wait()
	return len(batch), nil
}

func (n *Notifier) deliver(ctx context.Context, c claimed) error {
	var url string
	var enabled, platform bool
	var timeout int
	var nets []netip.Prefix
	var envelope []byte
	err := n.o.System.QueryRow(ctx, `
		SELECT w.url, w.enabled, w.timeout_seconds, t.is_platform, t.allowed_target_networks, e.envelope
		  FROM webhook_endpoints w JOIN tenants t ON t.id = w.tenant_id JOIN events e ON e.id = $2
		 WHERE w.id = $1`, c.endpoint, c.event).Scan(&url, &enabled, &timeout, &platform, &nets, &envelope)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = n.o.System.Exec(ctx, `UPDATE webhook_deliveries SET status = 'cancelled' WHERE id = $1`, c.id)
		return err
	}
	if err != nil {
		return err
	}
	if !enabled {
		n.deliveries.WithLabelValues("cancelled").Inc()
		_, err = n.o.System.Exec(ctx, `UPDATE webhook_deliveries SET status = 'cancelled', last_error = 'endpoint disabled'
			WHERE id = $1`, c.id)
		return err
	}
	signing, err := n.o.Store.SigningFor(ctx, n.o.System, c.endpoint)
	if err != nil {
		return err
	}
	body, err := withRoute(envelope, c.route)
	if err != nil {
		return err
	}
	var env struct{ ID string }
	_ = json.Unmarshal(envelope, &env)
	target := Target{URL: url, Timeout: time.Duration(timeout) * time.Second, Signing: signing,
		Policy: &plugin.NetPolicy{Deny: n.o.Deny, Allow: nets, DenyPrivate: !platform}}
	a := n.o.Sender.Send(ctx, target, env.ID, body)
	return n.record(ctx, c, a)
}

// record stores an attempt's outcome and schedules a retry if needed.
func (n *Notifier) record(ctx context.Context, c claimed, a Attempt) error {
	errText := ""
	if a.Err != nil {
		errText = a.Err.Error()
	} else if !a.OK() {
		errText = fmt.Sprintf("HTTP %d", a.StatusCode)
	}
	code := &a.StatusCode
	if a.StatusCode == 0 {
		code = nil
	}
	switch {
	case a.OK():
		n.deliveries.WithLabelValues("delivered").Inc()
		_, err := n.o.System.Exec(ctx, `UPDATE webhook_deliveries SET status = 'delivered', attempts = attempts + 1,
			last_attempt_at = now(), last_status_code = $2, last_error = NULL, last_response = $3, delivered_at = now()
			WHERE id = $1`, c.id, code, a.Response)
		return err
	case a.Gone():
		// The receiver asked never to be called again: disable the endpoint.
		n.deliveries.WithLabelValues("gone").Inc()
		return pgx.BeginFunc(ctx, n.o.System, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'cancelled', attempts = attempts + 1,
				last_attempt_at = now(), last_status_code = 410, last_error = $2, last_response = $3 WHERE id = $1`,
				c.id, errText, a.Response); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET enabled = false,
				disabled_reason = 'the receiver answered 410 Gone', updated_at = now() WHERE id = $1`, c.endpoint)
			return err
		})
	}
	attempts := c.attempts + 1
	giveUp := n.o.Config.GiveUpAfter.D()
	if giveUp > 0 && time.Since(c.created) >= giveUp {
		n.deliveries.WithLabelValues("failed").Inc()
		_, err := n.o.System.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', attempts = $2,
			last_attempt_at = now(), last_status_code = $3, last_error = $4, last_response = $5 WHERE id = $1`,
			c.id, attempts, code, errText, a.Response)
		return err
	}
	n.deliveries.WithLabelValues("retry").Inc()
	_, err := n.o.System.Exec(ctx, `UPDATE webhook_deliveries SET attempts = $2, last_attempt_at = now(),
		last_status_code = $3, last_error = $4, last_response = $5, next_attempt_at = now() + make_interval(secs => $6)
		WHERE id = $1`, c.id, attempts, code, errText, a.Response, n.backoff(attempts).Seconds())
	return err
}

// backoff is the configured retry schedule with ±20 % jitter; after the
// schedule runs out its last step repeats.
func (n *Notifier) backoff(attempts int) time.Duration {
	s := n.o.Config.RetrySchedule
	d := time.Hour
	if len(s) > 0 {
		d = s[min(attempts-1, len(s)-1)].D()
	}
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // jitter, not security
}
