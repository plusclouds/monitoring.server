package incident

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// OpenInput describes a new incident.
type OpenInput struct {
	TenantID   uuid.UUID
	CheckID    uuid.UUID
	DeviceID   uuid.UUID
	ObjectKey  *string // collectors: the object the incident is about
	ObjectName *string
	// Availability: the incident says its device is down (a host check, or
	// an availability object); it can suppress what depends on the device.
	Availability bool
	Severity     string
	Summary      string
	LastOutput   string
	RuleID       *string
	RuleName     *string
	Flapping     bool
}

// Open creates an incident and its opened event.
func Open(ctx context.Context, tx pgx.Tx, in OpenInput) (Incident, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Incident{}, err
	}
	// A failure explained by an upstream one is opened suppressed (F05).
	root, err := FindRoot(ctx, tx, in.DeviceID, in.CheckID, in.ObjectKey)
	if err != nil {
		return Incident{}, err
	}
	rootIncident, rootDevice := (*uuid.UUID)(nil), in.DeviceID
	if root != nil {
		rootIncident, rootDevice = &root.IncidentID, root.DeviceID
	}
	inc, err := scan(tx.QueryRow(ctx, `
		INSERT INTO incidents (id, tenant_id, check_id, device_id, severity, status, summary, last_output,
		                       rule_id, rule_name, flapping, suppressed, root_incident_id, root_device_id, opened_at,
		                       object_key, object_name, availability)
		VALUES ($1, $2, $3, $4, $5, 'open', $6, $7, $8, $9, $10, $11, $12, $13, now(), $14, $15, $16) RETURNING `+cols,
		id, in.TenantID, in.CheckID, in.DeviceID, in.Severity, in.Summary, in.LastOutput, in.RuleID,
		in.RuleName, in.Flapping, root != nil, rootIncident, rootDevice, in.ObjectKey, in.ObjectName, in.Availability))
	if err != nil {
		return Incident{}, err
	}
	return inc, emit(ctx, tx, EventOpened, inc, nil, nil)
}

// UpdateInput changes an open incident: severity, flapping or the cause.
type UpdateInput struct {
	Severity   string
	Summary    string
	LastOutput string
	RuleID     *string
	RuleName   *string
	Flapping   bool
}

// Update records a severity or flapping change and emits an updated event.
func Update(ctx context.Context, tx pgx.Tx, id uuid.UUID, in UpdateInput) (Incident, error) {
	inc, err := scan(tx.QueryRow(ctx, `
		UPDATE incidents SET severity = $2, summary = $3, last_output = $4, rule_id = $5, rule_name = $6,
		       flapping = $7, updated_at = now()
		 WHERE id = $1 AND status <> 'resolved' RETURNING `+cols,
		id, in.Severity, in.Summary, in.LastOutput, in.RuleID, in.RuleName, in.Flapping))
	if err != nil {
		return Incident{}, err
	}
	return inc, emit(ctx, tx, EventUpdated, inc, nil, nil)
}

// Resolve closes an incident. by is "recovery", "manual", "check-deleted",
// "check-disabled" or "object-gone". Resolving a resolved incident changes nothing.
func Resolve(ctx context.Context, tx pgx.Tx, id uuid.UUID, by string, actor *Actor) (Incident, bool, error) {
	cur, err := getForUpdate(ctx, tx, id)
	if err != nil || cur.Status == StatusResolved {
		return cur, false, err
	}
	inc, err := scan(tx.QueryRow(ctx, `
		UPDATE incidents SET status = 'resolved', resolved_at = now(), resolved_by = $2, flapping = false,
		       updated_at = now()
		 WHERE id = $1 RETURNING `+cols, id, by))
	if err != nil {
		return Incident{}, false, err
	}
	if err := emit(ctx, tx, EventResolved, inc, actor, nil); err != nil {
		return Incident{}, false, err
	}
	return inc, true, release(ctx, tx, inc.ID)
}

// Acknowledge marks an open incident as being handled; escalation stops
// but it stays open until recovery (F05). Acknowledging twice changes nothing.
func Acknowledge(ctx context.Context, tx pgx.Tx, id uuid.UUID, actor Actor) (Incident, bool, error) {
	cur, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return cur, false, err
	}
	switch cur.Status {
	case StatusAcknowledged:
		return cur, false, nil
	case StatusResolved:
		return cur, false, errs.Conflictf("incident-resolved", "the incident is already resolved")
	}
	inc, err := scan(tx.QueryRow(ctx, `
		UPDATE incidents SET status = 'acknowledged', acknowledged_at = now(), acknowledged_by = $2,
		       acknowledged_by_ext = $3, updated_at = now()
		 WHERE id = $1 RETURNING `+cols, id, actor.UserID, actor.ExternalID))
	if err != nil {
		return Incident{}, false, err
	}
	return inc, true, emit(ctx, tx, EventAcknowledged, inc, &actor, nil)
}

// AddComment adds a note to an incident and emits a commented event.
func AddComment(ctx context.Context, tx pgx.Tx, id uuid.UUID, actor Actor, body string) (Comment, error) {
	inc, err := Get(ctx, tx, id)
	if err != nil {
		return Comment{}, err
	}
	if body == "" || len(body) > 10000 {
		return Comment{}, errs.Invalidf("body", "must be 1 to 10000 characters")
	}
	cid, err := uuid.NewV7()
	if err != nil {
		return Comment{}, err
	}
	c := Comment{ID: cid, AuthorKind: actor.Kind, AuthorUserID: actor.UserID, AuthorExternalID: actor.ExternalID, Body: body}
	if err := tx.QueryRow(ctx, `INSERT INTO incident_comments (id, tenant_id, incident_id, author_kind, author_user_id,
		author_external_id, body) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		cid, inc.TenantID, id, actor.Kind, actor.UserID, actor.ExternalID, body).Scan(&c.CreatedAt); err != nil {
		return Comment{}, err
	}
	c.CreatedAt = c.CreatedAt.UTC()
	return c, emit(ctx, tx, EventCommented, inc, &actor, map[string]any{"comment": c})
}

// ResolveForChecks resolves the unresolved incidents of checks that are
// about to be deleted, so receivers see them end. checks is a SQL query
// returning check IDs, with args.
func ResolveForChecks(ctx context.Context, tx pgx.Tx, checks string, args ...any) error {
	return ResolveForChecksBy(ctx, tx, "check-deleted", checks, args...)
}

// ResolveForChecksBy resolves every open incident of the checks, objects'
// incidents included, with the given reason.
func ResolveForChecksBy(ctx context.Context, tx pgx.Tx, by, checks string, args ...any) error {
	rows, err := tx.Query(ctx, `SELECT id FROM incidents WHERE status <> 'resolved' AND check_id IN (`+checks+`)`, args...)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, _, err := Resolve(ctx, tx, id, by, nil); err != nil {
			return err
		}
	}
	return nil
}
