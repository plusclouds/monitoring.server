package api

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/webhook"
)

// GetNotificationCatalog lists the notifications a webhook can receive, each
// with an example payload (F06).
func (s *Server) GetNotificationCatalog(_ context.Context, _ gen.GetNotificationCatalogRequestObject) (gen.GetNotificationCatalogResponseObject, error) {
	events := webhook.Catalog()
	out := make([]map[string]any, len(events))
	for i, e := range events {
		var trigger *string
		if e.Trigger != "" {
			trigger = &e.Trigger
		}
		out[i] = map[string]any{"type": e.Type, "description": e.Description, "scope": e.Scope, "default": e.Default,
			"trigger": trigger, "example": e.Example}
	}
	res, err := via[gen.NotificationCatalog](map[string]any{
		"events": out, "match_fields": webhook.RouteMatchFields, "group_by_fields": webhook.GroupFields,
	})
	return gen.GetNotificationCatalog200JSONResponse(res), err
}

// ListDeviceNotifications shows the notification setup of a device: the
// routes that take its incidents, the health of their webhooks and the
// deliveries about the device (F06).
func (s *Server) ListDeviceNotifications(ctx context.Context, req gen.ListDeviceNotificationsRequestObject) (gen.ListDeviceNotificationsResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	q := req.Params
	n := 20
	if q.Limit != nil {
		n = min(max(*q.Limit, 1), 100)
	}
	f := webhook.DeviceDeliveryFilter{Limit: n + 1, EventType: q.EventType, IncidentURLTemplate: s.incidentURL}
	if q.Status != nil {
		v := string(*q.Status)
		f.Status = &v
	}
	first := q.Cursor == nil
	if !first {
		id, err := decodeCursorID(*q.Cursor)
		if err != nil {
			return nil, err
		}
		f.Before = &id
	}
	var routes []webhook.DeviceRoute
	var items []webhook.DeviceDelivery
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		// Evaluating also proves the device exists (404 otherwise).
		if routes, err = webhook.DeviceRoutes(ctx, tx, t.ID, req.DeviceId); err != nil {
			return err
		}
		items, err = webhook.DeviceDeliveries(ctx, tx, t.ID, req.DeviceId, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	next := (*string)(nil)
	if len(items) > n {
		items = items[:n]
		c := encodeCursorID(items[n-1].ID)
		next = &c
	}
	out := map[string]any{"routes": []any{}, "items": make([]any, len(items)), "next_cursor": next}
	if first {
		rs := make([]any, len(routes))
		for i, r := range routes {
			rs[i] = deviceRouteView(r)
		}
		out["routes"] = rs
	}
	for i, d := range items {
		out["items"].([]any)[i] = map[string]any{
			"id": d.ID, "event_type": d.EventType, "incident_id": d.IncidentID, "check_id": d.CheckID,
			"route_id": d.RouteID, "route_name": d.RouteName, "route_external": d.RouteExternal,
			"endpoint_id": d.EndpointID, "endpoint_name": d.EndpointName, "endpoint_external": d.EndpointExt, "status": d.Status, "attempts": d.Attempts,
			"last_status_code": d.LastStatusCode, "last_error": d.LastError, "last_attempt_at": d.LastAttemptAt,
			"delivered_at": d.DeliveredAt, "created_at": d.CreatedAt, "payload": json.RawMessage(d.Payload),
		}
	}
	res, err := via[gen.DeviceNotifications](out)
	return gen.ListDeviceNotifications200JSONResponse(res), err
}

func deviceRouteView(r webhook.DeviceRoute) map[string]any {
	checks := make([]map[string]any, len(r.Matches))
	for i, m := range r.Matches {
		checks[i] = map[string]any{"check_id": m.CheckID, "name": m.Name, "plugin": m.Plugin, "severities": m.Severities}
	}
	var reason *string
	if r.Reason != "" {
		reason = &r.Reason
	}
	h := r.Health
	status := h.LastStatus
	if status != nil && *status == "cancelled" {
		status = nil
	}
	return map[string]any{
		"route_id": r.Route.ID, "route_external": r.Route.External, "name": r.Route.Name, "position": r.Route.Position, "enabled": r.Route.Enabled,
		"match": r.Route.Match,
		"endpoint": map[string]any{"id": r.Route.EndpointID, "name": r.Endpoint.Name, "enabled": r.Endpoint.Enabled,
			"disabled_reason": r.Endpoint.DisabledReason, "external": r.Endpoint.External},
		"matches_because": nonNilSlice(r.Because), "matched_checks": checks,
		"health": map[string]any{"last_attempt_at": h.LastAttemptAt, "last_status": status,
			"last_status_code": h.LastStatusCode, "last_error": h.LastError, "failed_24h": h.Failed24h, "failing": h.Failing},
		"would_fire": r.WouldFire, "reason": reason,
	}
}
