package webhook

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/extref"
	"github.com/plusclouds/monitoring.server/internal/incident"
)

// RoutedEvents are the event types routes deliver by default: the incident
// lifecycle. Comments reach clients through the event stream only.
var RoutedEvents = []string{incident.EventOpened, incident.EventUpdated, incident.EventAcknowledged, incident.EventResolved}

// Match selects events for a route. Empty fields match everything; every
// set field must match.
type Match struct {
	Severity    []string          `json:"severity,omitempty"`
	DeviceTypes []string          `json:"device_types,omitempty"`
	SiteIDs     []uuid.UUID       `json:"site_ids,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	EventTypes  []string          `json:"event_types,omitempty"`
	// Per-alarm routing (v0.3.1): a customer points one check or one device
	// at its own webhook.
	CheckIDs  []uuid.UUID `json:"check_ids,omitempty"`
	DeviceIDs []uuid.UUID `json:"device_ids,omitempty"`
}

// Route sends matching events to an endpoint. Routes are evaluated by
// position; the first match wins unless it has Continue (F06).
type Route struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Name       string
	Position   int
	Enabled    bool
	Match      Match
	EndpointID uuid.UUID
	Continue   bool
	Labels     map[string]string // step labels; receivers decide what they mean
	ManagedBy  string
	External   *extref.Ref
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// RouteInput is the client-writable part of a route. Position 0 appends.
type RouteInput struct {
	Name       string
	Position   int
	Enabled    bool
	Match      Match
	EndpointID uuid.UUID
	Continue   bool
	Labels     map[string]string
	External   *extref.Ref
}

var knownEvents = []string{incident.EventOpened, incident.EventUpdated, incident.EventAcknowledged,
	incident.EventResolved, incident.EventCommented}

func (in *RouteInput) validate(ctx context.Context, tx pgx.Tx) error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 {
		return errs.Invalidf("name", "must be 1 to 200 characters")
	}
	for _, s := range in.Match.Severity {
		if s != "warning" && s != "critical" {
			return errs.Invalidf("match.severity", "%q is not warning or critical", s)
		}
	}
	for _, t := range in.Match.EventTypes {
		if !slices.Contains(knownEvents, t) {
			return errs.Invalidf("match.event_types", "%q is not one of %v", t, knownEvents)
		}
	}
	if len(in.Match.CheckIDs) > 100 || len(in.Match.DeviceIDs) > 100 {
		return errs.Invalidf("match", "at most 100 check_ids and 100 device_ids")
	}
	if len(in.Labels) > 20 {
		return errs.Invalidf("labels", "at most 20 labels")
	}
	if _, err := GetEndpoint(ctx, tx, in.EndpointID); errors.Is(err, errs.ErrNotFound) {
		return errs.Invalidf("endpoint_id", "webhook endpoint %s does not exist", in.EndpointID)
	} else if err != nil {
		return err
	}
	return in.External.Validate()
}

const routeCols = `id, tenant_id, name, position, enabled, match, endpoint_id, continue, labels, managed_by,
	external_source, external_type, external_id, created_at, updated_at`

var routeConstraints = map[string]string{
	"alert_routes_tenant_id_name_key": "an alert route with this name already exists",
	"alert_routes_external":           "an alert route with this external ID already exists",
}

func scanRoute(row pgx.Row) (Route, error) {
	var r Route
	var src, typ, ext *string
	err := row.Scan(&r.ID, &r.TenantID, &r.Name, &r.Position, &r.Enabled, &r.Match, &r.EndpointID, &r.Continue,
		&r.Labels, &r.ManagedBy, &src, &typ, &ext, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errs.ErrNotFound
	}
	r.External = extref.FromColumns(src, typ, ext)
	return r, err
}

func (r Route) snapshot() map[string]any {
	return map[string]any{"name": r.Name, "position": r.Position, "enabled": r.Enabled, "match": r.Match,
		"endpoint_id": r.EndpointID, "continue": r.Continue, "labels": r.Labels, "external": r.External}
}

// GetRoute returns one route.
func GetRoute(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Route, error) {
	return scanRoute(tx.QueryRow(ctx, `SELECT `+routeCols+` FROM alert_routes WHERE id = $1`, id))
}

// RouteByExternal finds a route by external ID.
func RouteByExternal(ctx context.Context, tx pgx.Tx, k extref.Key) (Route, error) {
	rows, err := tx.Query(ctx, `SELECT `+routeCols+` FROM alert_routes WHERE `+extref.Where(1)+` LIMIT 2`, k.Args()...)
	if err != nil {
		return Route{}, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Route, error) { return scanRoute(r) })
	switch {
	case err != nil:
		return Route{}, err
	case len(list) == 0:
		return Route{}, errs.ErrNotFound
	case len(list) > 1:
		return Route{}, extref.ErrAmbiguous
	}
	return list[0], nil
}

// ListRoutes returns the routes of the tenant in evaluation order. q may be
// a transaction scoped to the tenant or the system pool with tenant set.
func ListRoutes(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, tenant uuid.UUID) ([]Route, error) {
	rows, err := q.Query(ctx, `SELECT `+routeCols+` FROM alert_routes WHERE tenant_id = $1 ORDER BY position, id`, tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Route, error) { return scanRoute(r) })
}

// CreateRoute adds a route; position 0 places it last.
func CreateRoute(ctx context.Context, tx pgx.Tx, actor audit.Actor, tenant uuid.UUID, in RouteInput) (Route, error) {
	if err := in.validate(ctx, tx); err != nil {
		return Route{}, err
	}
	if in.Position == 0 {
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(position), 0) + 10 FROM alert_routes WHERE tenant_id = $1`, tenant).
			Scan(&in.Position); err != nil {
			return Route{}, err
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Route{}, err
	}
	src, typ, ext := in.External.Columns()
	r, err := scanRoute(tx.QueryRow(ctx, `
		INSERT INTO alert_routes (id, tenant_id, name, position, enabled, match, endpoint_id, continue, labels,
		                          external_source, external_type, external_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING `+routeCols,
		id, tenant, in.Name, in.Position, in.Enabled, in.Match, in.EndpointID, in.Continue, nonNilMap(in.Labels),
		src, typ, ext))
	if err != nil {
		return Route{}, errs.FromDB(err, routeConstraints)
	}
	return r, audit.Write(ctx, tx, actor.Event(tenant, "alert_route.create", "alert_route", id.String(), nil, r.snapshot()))
}

// UpdateRoute replaces a route; position 0 keeps the current position.
func UpdateRoute(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, in RouteInput) (Route, error) {
	cur, err := scanRoute(tx.QueryRow(ctx, `SELECT `+routeCols+` FROM alert_routes WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return Route{}, err
	}
	if err := in.validate(ctx, tx); err != nil {
		return Route{}, err
	}
	if in.Position == 0 {
		in.Position = cur.Position
	}
	next := cur
	next.Name, next.Position, next.Enabled, next.Match = in.Name, in.Position, in.Enabled, in.Match
	next.EndpointID, next.Continue, next.Labels, next.External = in.EndpointID, in.Continue, nonNilMap(maps.Clone(in.Labels)), in.External
	before, after := normalize(cur.snapshot()), normalize(next.snapshot())
	if reflect.DeepEqual(before, after) {
		return cur, nil
	}
	src, typ, ext := next.External.Columns()
	out, err := scanRoute(tx.QueryRow(ctx, `
		UPDATE alert_routes SET name = $2, position = $3, enabled = $4, match = $5, endpoint_id = $6, continue = $7,
		       labels = $8, external_source = $9, external_type = $10, external_id = $11, updated_at = now()
		 WHERE id = $1 RETURNING `+routeCols,
		id, next.Name, next.Position, next.Enabled, next.Match, next.EndpointID, next.Continue, next.Labels, src, typ, ext))
	if err != nil {
		return Route{}, errs.FromDB(err, routeConstraints)
	}
	return out, audit.Write(ctx, tx, actor.Event(cur.TenantID, "alert_route.update", "alert_route", id.String(), before, after))
}

// DeleteRoute removes a route.
func DeleteRoute(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID) error {
	cur, err := GetRoute(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM alert_routes WHERE id = $1`, id); err != nil {
		return err
	}
	return audit.Write(ctx, tx, actor.Event(cur.TenantID, "alert_route.delete", "alert_route", id.String(), cur.snapshot(), nil))
}

// Matches reports whether the route takes an event.
func (r Route) Matches(e *incident.Envelope, severity string) bool {
	m := r.Match
	types := m.EventTypes
	if len(types) == 0 {
		types = RoutedEvents
	}
	if !slices.Contains(types, e.Type) {
		return false
	}
	if len(m.Severity) > 0 && !slices.Contains(m.Severity, severity) {
		return false
	}
	d := e.Data.Device
	if len(m.DeviceTypes) > 0 && (d == nil || !slices.Contains(m.DeviceTypes, d.Type)) {
		return false
	}
	if len(m.SiteIDs) > 0 && (d == nil || d.SiteID == nil || !slices.Contains(m.SiteIDs, *d.SiteID)) {
		return false
	}
	if len(m.DeviceIDs) > 0 && (d == nil || !slices.Contains(m.DeviceIDs, d.ID)) {
		return false
	}
	if c := e.Data.Check; len(m.CheckIDs) > 0 && (c == nil || !slices.Contains(m.CheckIDs, c.ID)) {
		return false
	}
	for k, v := range m.Tags {
		if d == nil || d.Tags[k] != v {
			return false
		}
	}
	return true
}
