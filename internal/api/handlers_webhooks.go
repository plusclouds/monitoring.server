package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/webhook"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func toAPIWebhook(e webhook.Endpoint) gen.Webhook {
	var prev *time.Time
	if e.PreviousExpiresAt != nil && time.Now().Before(*e.PreviousExpiresAt) {
		u := e.PreviousExpiresAt.UTC()
		prev = &u
	}
	return gen.Webhook{
		Id: e.ID, Name: e.Name, Url: e.URL, Enabled: e.Enabled, DisabledReason: e.DisabledReason,
		TimeoutSeconds: e.TimeoutSeconds, HeaderNames: nonNilSlice(e.HeaderNames), PreviousValidUntil: prev,
		ManagedBy: e.ManagedBy, External: toAPIExternal(e.External),
		CreatedAt: e.CreatedAt.UTC(), UpdatedAt: e.UpdatedAt.UTC(),
	}
}

func withSecret(e webhook.Endpoint, secret string) (gen.WebhookWithSecret, error) {
	out, err := via[gen.WebhookWithSecret](toAPIWebhook(e))
	if secret != "" {
		out.Secret = &secret
	}
	return out, err
}

func webhookInput(b gen.WebhookWrite) webhook.EndpointInput {
	in := webhook.EndpointInput{Name: b.Name, URL: b.Url, Enabled: true, External: fromAPIExternal(b.External)}
	deref(&in.Enabled, b.Enabled)
	deref(&in.TimeoutSeconds, b.TimeoutSeconds)
	if b.Headers != nil {
		in.Headers = *b.Headers
	}
	return in
}

func (s *Server) ListWebhooks(ctx context.Context, _ gen.ListWebhooksRequestObject) (gen.ListWebhooksResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var items []webhook.Endpoint
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		items, err = webhook.ListEndpoints(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := gen.ListWebhooks200JSONResponse{Items: make([]gen.Webhook, len(items))}
	for i, e := range items {
		out.Items[i] = toAPIWebhook(e)
	}
	return out, nil
}

func (s *Server) CreateWebhook(ctx context.Context, req gen.CreateWebhookRequestObject) (gen.CreateWebhookResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	var e webhook.Endpoint
	var secret string
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		e, secret, err = s.hooks.CreateEndpoint(ctx, tx, p.Actor, t.ID, webhookInput(*req.Body))
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := withSecret(e, secret)
	return gen.CreateWebhook201JSONResponse(out), err
}

func (s *Server) UpsertWebhook(ctx context.Context, req gen.UpsertWebhookRequestObject) (gen.UpsertWebhookResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	k := s.key(req.Params.Source, req.Params.Type, req.ExternalId)
	in := webhookInput(*req.Body)
	in.External = k.Ref()
	var e webhook.Endpoint
	var secret string
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err := webhook.EndpointByExternal(ctx, tx, k)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			e, secret, err = s.hooks.CreateEndpoint(ctx, tx, p.Actor, t.ID, in)
		case err == nil:
			in.External = keepType(cur.External, k)
			e, err = s.hooks.UpdateEndpoint(ctx, tx, p.Actor, cur.ID, in)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := withSecret(e, secret)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		return gen.UpsertWebhook201JSONResponse(out), nil
	}
	return gen.UpsertWebhook200JSONResponse(out), nil
}

func (s *Server) GetWebhook(ctx context.Context, req gen.GetWebhookRequestObject) (gen.GetWebhookResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var e webhook.Endpoint
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		e, err = webhook.GetEndpoint(ctx, tx, req.WebhookId)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.GetWebhook200JSONResponse(toAPIWebhook(e)), nil
}

func (s *Server) UpdateWebhook(ctx context.Context, req gen.UpdateWebhookRequestObject) (gen.UpdateWebhookResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	var e webhook.Endpoint
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		e, err = s.hooks.UpdateEndpoint(ctx, tx, p.Actor, req.WebhookId, webhookInput(*req.Body))
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.UpdateWebhook200JSONResponse(toAPIWebhook(e)), nil
}

func (s *Server) DeleteWebhook(ctx context.Context, req gen.DeleteWebhookRequestObject) (gen.DeleteWebhookResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, t, func(tx pgx.Tx) error { return webhook.DeleteEndpoint(ctx, tx, p.Actor, req.WebhookId) })
	if err != nil {
		return nil, err
	}
	return gen.DeleteWebhook204Response{}, nil
}

func (s *Server) RotateWebhookSecret(ctx context.Context, req gen.RotateWebhookSecretRequestObject) (gen.RotateWebhookSecretResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	var e webhook.Endpoint
	var secret string
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		e, secret, err = s.hooks.RotateSecret(ctx, tx, p.Actor, req.WebhookId, s.rotationOverlap)
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := withSecret(e, secret)
	return gen.RotateWebhookSecret200JSONResponse(out), err
}

// TestWebhook sends a signed test event synchronously (F06).
func (s *Server) TestWebhook(ctx context.Context, req gen.TestWebhookRequestObject) (gen.TestWebhookResponseObject, error) {
	p, t, err := tenantScope(ctx, roleOperator, true)
	if err != nil {
		return nil, err
	}
	var e webhook.Endpoint
	var signing webhook.Signing
	var installation uuid.UUID
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if e, err = webhook.GetEndpoint(ctx, tx, req.WebhookId); err != nil {
			return err
		}
		if signing, err = s.hooks.SigningFor(ctx, tx, e.ID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM installation`).Scan(&installation); err != nil {
			return err
		}
		return audit.Write(ctx, tx, p.Actor.Event(t.ID, "webhook.test", "webhook", e.ID.String(), nil, nil))
	})
	if err != nil {
		return nil, err
	}
	id := uuid.Must(uuid.NewV7())
	body, err := json.Marshal(map[string]any{
		"specversion": "1.0", "id": id, "type": "monitoring.webhook.test", "time": time.Now().UTC(),
		"source": "/monitoring/" + installation.String() + "/" + t.ID.String(), "subject": e.ID,
		"datacontenttype": "application/json",
		"data": map[string]any{"account_id": t.ExternalID, "object_type": `Monitoring\Webhooks`,
			"object": toAPIWebhook(e), "actor": incidentActor(p)},
	})
	if err != nil {
		return nil, err
	}
	a := s.sender.Send(ctx, webhook.Target{
		URL: e.URL, Timeout: time.Duration(e.TimeoutSeconds) * time.Second, Signing: signing,
		Policy: &plugin.NetPolicy{Deny: s.exec.Deny, Allow: t.Limits.AllowedTargetNetworks, DenyPrivate: !t.IsPlatform},
	}, id.String(), body)
	out := gen.TestWebhook200JSONResponse{Ok: a.OK(), Response: a.Response}
	if a.StatusCode != 0 {
		out.StatusCode = &a.StatusCode
	}
	if a.Err != nil {
		msg := a.Err.Error()
		out.Error = &msg
	}
	return out, nil
}

const deliveryCols = `id, event_id, event_type, subject, route_id, status, attempts, next_attempt_at, last_attempt_at,
	last_status_code, last_error, last_response, delivered_at, created_at`

func scanDelivery(r pgx.Row) (gen.WebhookDelivery, error) {
	var d gen.WebhookDelivery
	var status string
	err := r.Scan(&d.Id, &d.EventId, &d.EventType, &d.Subject, &d.RouteId, &status, &d.Attempts, &d.NextAttemptAt,
		&d.LastAttemptAt, &d.LastStatusCode, &d.LastError, &d.LastResponse, &d.DeliveredAt, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, errs.ErrNotFound
	}
	d.Status = gen.WebhookDeliveryStatus(status)
	d.NextAttemptAt, d.CreatedAt = d.NextAttemptAt.UTC(), d.CreatedAt.UTC()
	for _, p := range []**time.Time{&d.LastAttemptAt, &d.DeliveredAt} {
		if *p != nil {
			u := (**p).UTC()
			*p = &u
		}
	}
	return d, err
}

func (s *Server) ListWebhookDeliveries(ctx context.Context, req gen.ListWebhookDeliveriesRequestObject) (gen.ListWebhookDeliveriesResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	q := req.Params
	n := 100
	if q.Limit != nil {
		n = *q.Limit
	}
	var before *uuid.UUID
	if q.Cursor != nil {
		id, err := decodeCursorID(*q.Cursor)
		if err != nil {
			return nil, err
		}
		before = &id
	}
	var status *string
	if q.Status != nil {
		v := string(*q.Status)
		status = &v
	}
	var items []gen.WebhookDelivery
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if _, err := webhook.GetEndpoint(ctx, tx, req.WebhookId); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
			 WHERE endpoint_id = $1 AND ($2::text IS NULL OR status = $2) AND ($3::uuid IS NULL OR id < $3)
			 ORDER BY id DESC LIMIT $4`, req.WebhookId, status, before, n+1)
		if err != nil {
			return err
		}
		items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (gen.WebhookDelivery, error) { return scanDelivery(r) })
		return err
	})
	if err != nil {
		return nil, err
	}
	items, next := trim(items, n, func(d gen.WebhookDelivery) uuid.UUID { return d.Id })
	return gen.ListWebhookDeliveries200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) ReplayWebhookDelivery(ctx context.Context, req gen.ReplayWebhookDeliveryRequestObject) (gen.ReplayWebhookDeliveryResponseObject, error) {
	p, t, err := tenantScope(ctx, roleOperator, true)
	if err != nil {
		return nil, err
	}
	var d gen.WebhookDelivery
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		d, err = scanDelivery(tx.QueryRow(ctx, `UPDATE webhook_deliveries
			SET status = 'pending', next_attempt_at = now(), created_at = now()
			WHERE id = $1 AND endpoint_id = $2 AND status <> 'pending' RETURNING `+deliveryCols, req.DeliveryId, req.WebhookId))
		if errors.Is(err, errs.ErrNotFound) {
			if _, gerr := webhook.GetEndpoint(ctx, tx, req.WebhookId); gerr != nil {
				return gerr
			}
			return problem(http.StatusConflict, "delivery-pending", "Delivery pending",
				"The delivery does not exist or is already waiting to be sent.")
		}
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, p.Actor.Event(t.ID, "webhook.replay", "webhook", req.WebhookId.String(), nil,
			map[string]any{"delivery_id": req.DeliveryId}))
	})
	if err != nil {
		return nil, err
	}
	return gen.ReplayWebhookDelivery202JSONResponse(d), nil
}

// ---- alert routes ----

func toAPIRoute(r webhook.Route) (gen.AlertRoute, error) {
	m, err := via[gen.AlertRouteMatch](r.Match)
	labels := r.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return gen.AlertRoute{
		Id: r.ID, Name: r.Name, Position: r.Position, Enabled: r.Enabled, Match: m, EndpointId: r.EndpointID,
		Continue: r.Continue, Labels: labels, ManagedBy: r.ManagedBy, External: toAPIExternal(r.External),
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}, err
}

func routeInput(b gen.AlertRouteWrite) (webhook.RouteInput, error) {
	in := webhook.RouteInput{Name: b.Name, EndpointID: b.EndpointId, Enabled: true, External: fromAPIExternal(b.External)}
	deref(&in.Position, b.Position)
	deref(&in.Enabled, b.Enabled)
	deref(&in.Continue, b.Continue)
	if b.Labels != nil {
		in.Labels = *b.Labels
	}
	if b.Match != nil {
		m, err := via[webhook.Match](*b.Match)
		if err != nil {
			return in, err
		}
		in.Match = m
	}
	return in, nil
}

func (s *Server) ListAlertRoutes(ctx context.Context, _ gen.ListAlertRoutesRequestObject) (gen.ListAlertRoutesResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var items []webhook.Route
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		items, err = webhook.ListRoutes(ctx, tx, t.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := gen.ListAlertRoutes200JSONResponse{Items: make([]gen.AlertRoute, len(items))}
	for i, r := range items {
		if out.Items[i], err = toAPIRoute(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) CreateAlertRoute(ctx context.Context, req gen.CreateAlertRouteRequestObject) (gen.CreateAlertRouteResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	in, err := routeInput(*req.Body)
	if err != nil {
		return nil, err
	}
	var r webhook.Route
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		r, err = webhook.CreateRoute(ctx, tx, p.Actor, t.ID, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := toAPIRoute(r)
	return gen.CreateAlertRoute201JSONResponse(out), err
}

func (s *Server) UpsertAlertRoute(ctx context.Context, req gen.UpsertAlertRouteRequestObject) (gen.UpsertAlertRouteResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	k := s.key(req.Params.Source, req.Params.Type, req.ExternalId)
	in, err := routeInput(*req.Body)
	if err != nil {
		return nil, err
	}
	in.External = k.Ref()
	var r webhook.Route
	var created bool
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err := webhook.RouteByExternal(ctx, tx, k)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			created = true
			r, err = webhook.CreateRoute(ctx, tx, p.Actor, t.ID, in)
		case err == nil:
			in.External = keepType(cur.External, k)
			r, err = webhook.UpdateRoute(ctx, tx, p.Actor, cur.ID, in)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := toAPIRoute(r)
	if err != nil {
		return nil, err
	}
	if created {
		return gen.UpsertAlertRoute201JSONResponse(out), nil
	}
	return gen.UpsertAlertRoute200JSONResponse(out), nil
}

func (s *Server) GetAlertRoute(ctx context.Context, req gen.GetAlertRouteRequestObject) (gen.GetAlertRouteResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var r webhook.Route
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		r, err = webhook.GetRoute(ctx, tx, req.RouteId)
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := toAPIRoute(r)
	return gen.GetAlertRoute200JSONResponse(out), err
}

func (s *Server) UpdateAlertRoute(ctx context.Context, req gen.UpdateAlertRouteRequestObject) (gen.UpdateAlertRouteResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	in, err := routeInput(*req.Body)
	if err != nil {
		return nil, err
	}
	var r webhook.Route
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		r, err = webhook.UpdateRoute(ctx, tx, p.Actor, req.RouteId, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := toAPIRoute(r)
	return gen.UpdateAlertRoute200JSONResponse(out), err
}

func (s *Server) DeleteAlertRoute(ctx context.Context, req gen.DeleteAlertRouteRequestObject) (gen.DeleteAlertRouteResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, t, func(tx pgx.Tx) error { return webhook.DeleteRoute(ctx, tx, p.Actor, req.RouteId) })
	if err != nil {
		return nil, err
	}
	return gen.DeleteAlertRoute204Response{}, nil
}
