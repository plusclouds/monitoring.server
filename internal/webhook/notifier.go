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

	lastCleanup time.Time
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
	if err := n.flushGroups(ctx); err != nil && ctx.Err() == nil {
		n.log.Error("flush alert groups", "error", err)
	}
	if err := n.repeat(ctx); err != nil && ctx.Err() == nil {
		n.log.Error("repeat notifications", "error", err)
	}
	if err := n.escalate(ctx); err != nil && ctx.Err() == nil {
		n.log.Error("escalate incidents", "error", err)
	}
	if time.Since(n.lastCleanup) > time.Hour {
		n.lastCleanup = time.Now()
		if _, err := n.Cleanup(ctx); err != nil && ctx.Err() == nil {
			n.log.Error("delivery retention", "error", err)
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

// unrouted is an event waiting for the router.
type unrouted struct {
	id, tenant uuid.UUID
	seq        int64
	typ        string
	subject    *string
	raw        []byte
	held       bool // route_after was set: the dependency grace has passed
}

// route turns a batch of unrouted events into deliveries and group
// members. An opened incident with something upstream waits for the
// dependency grace, and the later events of its incident wait with it; a
// suppressed incident is not notified (F05).
func (n *Notifier) route(ctx context.Context) (int, error) {
	var count int
	err := pgx.BeginFunc(ctx, n.o.System, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, seq, type, subject, envelope, route_after IS NOT NULL FROM events
			WHERE routed_at IS NULL AND (route_after IS NULL OR route_after <= now())
			ORDER BY seq LIMIT $1 FOR UPDATE SKIP LOCKED`, max(n.o.Config.ClaimBatch, 1))
		if err != nil {
			return err
		}
		evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (unrouted, error) {
			var e unrouted
			err := r.Scan(&e.id, &e.tenant, &e.seq, &e.typ, &e.subject, &e.raw, &e.held)
			return e, err
		})
		if err != nil || len(evs) == 0 {
			return err
		}
		count = len(evs)
		routes := map[uuid.UUID][]Route{}
		var done []uuid.UUID
		for _, e := range evs {
			wait, err := n.hold(ctx, tx, e)
			if err != nil {
				return err
			}
			if !wait.IsZero() {
				if _, err := tx.Exec(ctx, `UPDATE events SET route_after = $2 WHERE id = $1`, e.id, wait); err != nil {
					return err
				}
				continue
			}
			done = append(done, e.id)
			var env incident.Envelope
			if err := json.Unmarshal(e.raw, &env); err != nil {
				n.log.Error("unreadable event", "event_id", e.id, "error", err)
				continue
			}
			obj, _ := env.Data.Object.(map[string]any)
			if suppressed, _ := obj["suppressed"].(bool); suppressed {
				continue
			}
			if e.held && e.typ == incident.EventOpened && e.subject != nil {
				// The grace has passed: an upstream incident may explain this one now.
				id, err := uuid.Parse(*e.subject)
				if err != nil {
					continue
				}
				if suppressed, err := incident.Suppress(ctx, tx, id); err != nil {
					return err
				} else if suppressed {
					continue
				}
			}
			rs, ok := routes[e.tenant]
			if !ok {
				if rs, err = ListRoutes(ctx, tx, e.tenant); err != nil {
					return err
				}
				routes[e.tenant] = rs
			}
			severity, _ := obj["severity"].(string)
			for _, r := range rs {
				if !r.Enabled || !r.Matches(&env, severity) {
					continue
				}
				if err := n.take(ctx, tx, e, &env, r); err != nil {
					return err
				}
				if !r.Continue {
					break
				}
			}
		}
		if len(done) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE events SET routed_at = now() WHERE id = ANY($1)`, done)
		return err
	})
	return count, err
}

// hold returns when an event may be routed, or zero for now: after an
// earlier held event of the same incident, or, for an incident just opened
// with something upstream, after the dependency grace.
func (n *Notifier) hold(ctx context.Context, tx pgx.Tx, e unrouted) (time.Time, error) {
	if e.subject == nil || *e.subject == "" {
		return time.Time{}, nil
	}
	var earlier *time.Time
	if err := tx.QueryRow(ctx, `SELECT max(route_after) FROM events
		WHERE subject = $1 AND routed_at IS NULL AND seq < $2 AND tenant_id = $3`, *e.subject, e.seq, e.tenant).
		Scan(&earlier); err != nil {
		return time.Time{}, err
	}
	if earlier != nil && earlier.After(time.Now()) {
		return *earlier, nil
	}
	grace := n.o.Config.DependencyGrace.D()
	if e.held || e.typ != incident.EventOpened || grace <= 0 {
		return time.Time{}, nil
	}
	var device, check *uuid.UUID
	var opened time.Time
	err := tx.QueryRow(ctx, `SELECT device_id, check_id, opened_at FROM incidents WHERE id::text = $1`, *e.subject).
		Scan(&device, &check, &opened)
	if errors.Is(err, pgx.ErrNoRows) || device == nil || check == nil {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	up, err := incident.HasUpstream(ctx, tx, *device, *check)
	if err != nil || !up || !opened.Add(grace).After(time.Now()) {
		return time.Time{}, err
	}
	return opened.Add(grace), nil
}

// take hands a matched event to a route: into a group when the route
// groups, else straight to a delivery. It records the notification for
// repeat_interval.
func (n *Notifier) take(ctx context.Context, tx pgx.Tx, e unrouted, env *incident.Envelope, r Route) error {
	if len(r.GroupBy) > 0 {
		if err := n.addToGroup(ctx, tx, e, env, r); err != nil {
			return err
		}
	} else if err := n.enqueue(ctx, tx, e.tenant, e.id, e.seq, e.typ, deref(e.subject), r); err != nil {
		return err
	}
	if e.subject == nil {
		return nil
	}
	if steps := r.stepsOf(); e.typ == incident.EventOpened && len(steps) > 1 {
		for i, st := range steps[1:] {
			if _, err := tx.Exec(ctx, `INSERT INTO route_escalations (tenant_id, route_id, incident_id, step, due_at)
				SELECT $1, $2, id, $4, now() + make_interval(secs => $5) FROM incidents WHERE id::text = $3
				ON CONFLICT DO NOTHING`, e.tenant, r.ID, *e.subject, i+1, st.After.Seconds()); err != nil {
				return err
			}
		}
	}
	if r.RepeatInterval == 0 {
		return nil
	}
	switch e.typ {
	case incident.EventOpened, incident.EventUpdated:
		_, err := tx.Exec(ctx, `INSERT INTO route_notifications (tenant_id, route_id, incident_id, last_sent_at)
			SELECT $1, $2, id, now() FROM incidents WHERE id::text = $3
			ON CONFLICT (route_id, incident_id) DO UPDATE SET last_sent_at = now()`, e.tenant, r.ID, *e.subject)
		return err
	case incident.EventResolved:
		_, err := tx.Exec(ctx, `DELETE FROM route_notifications WHERE route_id = $1 AND incident_id::text = $2`, r.ID, *e.subject)
		return err
	}
	return nil
}

// enqueue creates a delivery of an event to a route's endpoint, unless the
// endpoint is disabled, with the first step's labels.
func (n *Notifier) enqueue(ctx context.Context, tx pgx.Tx, tenant, event uuid.UUID, seq int64, typ, subject string, r Route) error {
	return n.enqueueStep(ctx, tx, tenant, event, seq, typ, subject, r, 0)
}

func (n *Notifier) enqueueStep(ctx context.Context, tx pgx.Tx, tenant, event uuid.UUID, seq int64, typ, subject string,
	r Route, step int) error {
	var on bool
	if err := tx.QueryRow(ctx, `SELECT enabled FROM webhook_endpoints WHERE id = $1`, r.EndpointID).Scan(&on); err != nil || !on {
		return err
	}
	steps := r.stepsOf()
	labels := map[string]string{}
	if step < len(steps) {
		labels = nonNilMap(steps[step].Labels)
	}
	info, _ := json.Marshal(routeInfo{ID: r.ID, Name: r.Name, Step: step, Labels: labels})
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO webhook_deliveries (id, tenant_id, event_id, event_seq, event_type,
		subject, endpoint_id, route_id, route) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (event_id, endpoint_id) DO NOTHING`,
		id, tenant, event, seq, typ, subject, r.EndpointID, r.ID, json.RawMessage(info))
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
	body, err := render(envelope, c.route, n.o.Config.IncidentURLTemplate, c.tenant)
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
