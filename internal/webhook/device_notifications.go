package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/extref"
	"github.com/plusclouds/monitoring.server/internal/incident"
)

// Reasons a route would not notify for a device (F06).
const (
	ReasonRouteDisabled    = "route_disabled"
	ReasonNoMatch          = "no_match"
	ReasonEndpointDisabled = "endpoint_disabled"
	ReasonShadowed         = "shadowed"
	ReasonEndpointFailing  = "endpoint_failing"
)

// maxEvaluatedChecks bounds the checks of one device that are evaluated.
const maxEvaluatedChecks = 50

// Health is how deliveries to an endpoint have been going. It covers the
// whole endpoint, not one device.
type Health struct {
	LastAttemptAt  *time.Time
	LastStatus     *string
	LastStatusCode *int
	LastError      *string
	Failed24h      int
	// Failing: the latest delivery failed for good, or is being retried.
	Failing bool
}

// CheckMatch is a check of the device that a route takes, and for which
// severities.
type CheckMatch struct {
	CheckID    uuid.UUID
	Name       string
	Plugin     string
	Severities []string
}

// DeviceRoute is one alert route seen from one device.
type DeviceRoute struct {
	Route    Route
	Endpoint Endpoint
	Health   Health
	Matches  []CheckMatch
	// Because lists the route's filters that the device satisfies.
	Because   []string
	WouldFire bool
	// Reason is set when WouldFire is false.
	Reason string
}

// DeviceRoutes evaluates every route of the tenant against the next
// incident of the device: for each enabled check and each severity it walks
// the routes the way the router does (Evaluate) and records which routes
// take the incident and would deliver it. A device without checks is
// evaluated once, without a check.
func DeviceRoutes(ctx context.Context, tx pgx.Tx, tenant, device uuid.UUID) ([]DeviceRoute, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE id = $1)`, device).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errs.ErrNotFound
	}
	routes, err := ListRoutes(ctx, tx, tenant)
	if err != nil || len(routes) == 0 {
		return nil, err
	}
	endpoints, err := ListEndpoints(ctx, tx)
	if err != nil {
		return nil, err
	}
	byID := map[uuid.UUID]Endpoint{}
	ids := make([]uuid.UUID, 0, len(endpoints))
	for _, e := range endpoints {
		byID[e.ID] = e
		ids = append(ids, e.ID)
	}
	health, err := EndpointHealth(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id, name, plugin FROM checks WHERE device_id = $1 AND enabled
		ORDER BY name, id LIMIT $2`, device, maxEvaluatedChecks)
	if err != nil {
		return nil, err
	}
	checks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CheckMatch, error) {
		var c CheckMatch
		err := r.Scan(&c.CheckID, &c.Name, &c.Plugin)
		return c, err
	})
	if err != nil {
		return nil, err
	}

	out := make([]DeviceRoute, len(routes))
	reached := make([]bool, len(routes)) // delivers, endpoint on
	taken := make([]bool, len(routes))   // matched and routed, whatever the endpoint
	shadowed := make([]bool, len(routes))
	for i, r := range routes {
		out[i] = DeviceRoute{Route: r, Endpoint: byID[r.EndpointID], Health: health[r.EndpointID]}
	}

	scenarios := checks
	if len(scenarios) == 0 {
		scenarios = []CheckMatch{{}}
	}
	for _, c := range scenarios {
		var check *uuid.UUID
		if c.CheckID != uuid.Nil {
			check = &c.CheckID
		}
		env, err := incident.Preview(ctx, tx, tenant, incident.EventOpened, "critical", &device, check)
		if err != nil {
			return nil, err
		}
		for _, severity := range []string{"warning", "critical"} {
			stopped := false
			for i, r := range routes {
				if !r.Matches(&env, severity) {
					continue
				}
				if len(out[i].Because) == 0 {
					out[i].Because = because(r.Match, &env, severity)
				}
				if check != nil {
					addMatch(&out[i], c, severity)
				}
				if !r.Enabled {
					continue
				}
				if stopped {
					shadowed[i] = true
					continue
				}
				taken[i] = true
				if byID[r.EndpointID].Enabled {
					reached[i] = true
				}
				stopped = !r.Continue
			}
		}
	}

	for i := range out {
		switch {
		case !routes[i].Enabled:
			out[i].Reason = ReasonRouteDisabled
		case !taken[i] && shadowed[i]:
			out[i].Reason = ReasonShadowed
		case !taken[i]:
			out[i].Reason = ReasonNoMatch
		case !reached[i]:
			out[i].Reason = ReasonEndpointDisabled
		case out[i].Health.Failing:
			out[i].Reason = ReasonEndpointFailing
		default:
			out[i].WouldFire = true
		}
	}
	return out, nil
}

func addMatch(r *DeviceRoute, c CheckMatch, severity string) {
	for i := range r.Matches {
		if r.Matches[i].CheckID == c.CheckID {
			if !slices.Contains(r.Matches[i].Severities, severity) {
				r.Matches[i].Severities = append(r.Matches[i].Severities, severity)
			}
			return
		}
	}
	c.Severities = []string{severity}
	r.Matches = append(r.Matches, c)
}

// because lists, as short codes, the filters of m that the event satisfies.
// A route without filters takes every incident.
func because(m Match, e *incident.Envelope, severity string) []string {
	var out []string
	if len(m.EventTypes) > 0 {
		out = append(out, "event_type:"+e.Type)
	}
	if len(m.Severity) > 0 {
		out = append(out, "severity:"+severity)
	}
	if d := e.Data.Device; d != nil {
		if len(m.DeviceTypes) > 0 {
			out = append(out, "device_type:"+d.Type)
		}
		if len(m.SiteIDs) > 0 && d.SiteID != nil {
			out = append(out, "site:"+d.SiteID.String())
		}
		if len(m.DeviceIDs) > 0 {
			out = append(out, "device_id:"+d.ID.String())
		}
		for _, k := range sortedKeys(m.Tags) {
			out = append(out, "tag:"+k+"="+m.Tags[k])
		}
	}
	if c := e.Data.Check; c != nil && len(m.CheckIDs) > 0 {
		out = append(out, "check_id:"+c.ID.String())
	}
	if len(out) == 0 {
		out = []string{"all_incidents"}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// EndpointHealth summarises the recent deliveries of each endpoint.
func EndpointHealth(ctx context.Context, tx pgx.Tx, endpoints []uuid.UUID) (map[uuid.UUID]Health, error) {
	out := map[uuid.UUID]Health{}
	if len(endpoints) == 0 {
		return out, nil
	}
	// The latest delivery that was not cancelled gives the status.
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (endpoint_id) endpoint_id, status, attempts, last_status_code, last_error, last_attempt_at
		  FROM webhook_deliveries
		 WHERE endpoint_id = ANY($1) AND status <> 'cancelled'
		 ORDER BY endpoint_id, id DESC`, endpoints)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		var h Health
		var status string
		var attempts int
		if err := rows.Scan(&id, &status, &attempts, &h.LastStatusCode, &h.LastError, &h.LastAttemptAt); err != nil {
			rows.Close()
			return nil, err
		}
		h.LastStatus = &status
		h.Failing = status == "failed" || (status == "pending" && attempts > 0 && h.LastError != nil)
		out[id] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `
		SELECT endpoint_id, count(*) FILTER (WHERE status = 'failed'
		                                       AND coalesce(last_attempt_at, created_at) > now() - interval '24 hours'),
		       max(last_attempt_at)
		  FROM webhook_deliveries WHERE endpoint_id = ANY($1) GROUP BY endpoint_id`, endpoints)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var failed int
		var last *time.Time
		if err := rows.Scan(&id, &failed, &last); err != nil {
			return nil, err
		}
		h := out[id]
		h.Failed24h, h.LastAttemptAt = failed, last
		out[id] = h
	}
	return out, rows.Err()
}

// DeviceDelivery is a delivery about a device, with the body that was sent.
type DeviceDelivery struct {
	ID             uuid.UUID
	EventType      string
	IncidentID     *uuid.UUID
	CheckID        *uuid.UUID
	RouteID        *uuid.UUID
	RouteName      *string
	RouteExternal  *extref.Ref
	EndpointID     uuid.UUID
	EndpointName   string
	EndpointExt    *extref.Ref
	Status         string
	Attempts       int
	LastStatusCode *int
	LastError      *string
	LastAttemptAt  *time.Time
	DeliveredAt    *time.Time
	CreatedAt      time.Time
	Payload        json.RawMessage
}

// DeviceDeliveryFilter selects a page of a device's deliveries, newest
// first. Before is the ID of the last delivery of the previous page.
type DeviceDeliveryFilter struct {
	Status, EventType *string
	Before            *uuid.UUID
	Limit             int
	// IncidentURLTemplate is notifier.incident_url_template, so the payload
	// carries the same data.links as the delivered body.
	IncidentURLTemplate string
}

// DeviceDeliveries lists the deliveries that are about a device. A grouped
// event is listed for every device in the group. Payloads are rebuilt the
// way the sender builds them. A body holds no secret: the signature is in
// headers, which are not stored.
func DeviceDeliveries(ctx context.Context, tx pgx.Tx, tenant, device uuid.UUID, f DeviceDeliveryFilter) ([]DeviceDelivery, error) {
	rows, err := tx.Query(ctx, `
		SELECT d.id, d.event_type, d.route_id, r.name, r.external_source, r.external_type, r.external_id, d.endpoint_id, w.name,
		       w.external_source, w.external_type, w.external_id, d.status, d.attempts, d.last_status_code,
		       d.last_error, d.last_attempt_at, d.delivered_at, d.created_at, e.envelope, d.route
		  FROM webhook_delivery_devices x
		  JOIN webhook_deliveries d ON d.id = x.delivery_id
		  JOIN events e ON e.id = d.event_id
		  JOIN webhook_endpoints w ON w.id = d.endpoint_id
		  LEFT JOIN alert_routes r ON r.id = d.route_id
		 WHERE x.tenant_id = $1 AND x.device_id = $2
		   AND ($3::text IS NULL OR d.status = $3) AND ($4::text IS NULL OR d.event_type = $4)
		   AND ($5::uuid IS NULL OR d.id < $5)
		 ORDER BY d.id DESC LIMIT $6`, tenant, device, f.Status, f.EventType, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceDelivery
	for rows.Next() {
		var d DeviceDelivery
		var envelope, route []byte
		var rsrc, rtyp, rext, wsrc, wtyp, wext *string
		if err := rows.Scan(&d.ID, &d.EventType, &d.RouteID, &d.RouteName, &rsrc, &rtyp, &rext, &d.EndpointID,
			&d.EndpointName, &wsrc, &wtyp, &wext, &d.Status, &d.Attempts, &d.LastStatusCode, &d.LastError, &d.LastAttemptAt, &d.DeliveredAt, &d.CreatedAt, &envelope, &route); err != nil {
			return nil, err
		}
		d.RouteExternal, d.EndpointExt = extref.FromColumns(rsrc, rtyp, rext), extref.FromColumns(wsrc, wtyp, wext)
		body, err := render(envelope, route, f.IncidentURLTemplate, tenant)
		if err != nil {
			return nil, fmt.Errorf("render delivery %s: %w", d.ID, err)
		}
		d.Payload = body
		var env incident.Envelope
		if json.Unmarshal(envelope, &env) == nil {
			if id, err := uuid.Parse(env.Subject); err == nil {
				d.IncidentID = &id
			}
			if env.Data.Check != nil {
				id := env.Data.Check.ID
				d.CheckID = &id
			}
		}
		d.CreatedAt = d.CreatedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}
