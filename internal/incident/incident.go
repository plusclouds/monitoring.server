// Package incident stores incidents and writes their events (F05,
// ADR-0009). Every change to an incident and the CloudEvent describing it
// are written in the same transaction, so no event is lost or sent for a
// change that was rolled back. The engine, the API and inventory deletes
// share this package so every path emits the same events.
package incident

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Statuses.
const (
	StatusOpen         = "open"
	StatusAcknowledged = "acknowledged"
	StatusResolved     = "resolved"
)

// Incident is one problem episode of one check.
type Incident struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"-"`
	CheckID           *uuid.UUID `json:"check_id"`
	DeviceID          *uuid.UUID `json:"device_id"`
	Severity          string     `json:"severity"`
	Status            string     `json:"status"`
	Summary           string     `json:"summary"`
	LastOutput        *string    `json:"last_output"`
	RuleID            *string    `json:"rule_id"`
	RuleName          *string    `json:"rule_name"`
	Flapping          bool       `json:"flapping"`
	Suppressed        bool       `json:"suppressed"`
	RootIncidentID    *uuid.UUID `json:"root_incident_id"` // set while suppressed: the incident that explains this one
	RootDeviceID      *uuid.UUID `json:"root_device_id"`   // the root incident's device, or this incident's own
	OpenedAt          time.Time  `json:"opened_at"`
	AcknowledgedAt    *time.Time `json:"acknowledged_at"`
	AcknowledgedBy    *uuid.UUID `json:"acknowledged_by"`
	AcknowledgedByExt *string    `json:"acknowledged_by_external_id"`
	ResolvedAt        *time.Time `json:"resolved_at"`
	ResolvedBy        *string    `json:"resolved_by"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

const cols = `id, tenant_id, check_id, device_id, severity, status, summary, last_output, rule_id, rule_name,
	flapping, suppressed, root_incident_id, root_device_id, opened_at, acknowledged_at, acknowledged_by, acknowledged_by_ext, resolved_at,
	resolved_by, updated_at`

func scan(row pgx.Row) (Incident, error) {
	var i Incident
	err := row.Scan(&i.ID, &i.TenantID, &i.CheckID, &i.DeviceID, &i.Severity, &i.Status, &i.Summary,
		&i.LastOutput, &i.RuleID, &i.RuleName, &i.Flapping, &i.Suppressed, &i.RootIncidentID, &i.RootDeviceID, &i.OpenedAt, &i.AcknowledgedAt,
		&i.AcknowledgedBy, &i.AcknowledgedByExt, &i.ResolvedAt, &i.ResolvedBy, &i.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return i, errs.ErrNotFound
	}
	return i, err
}

// UTC normalizes timestamps for output.
func (i Incident) UTC() Incident {
	i.OpenedAt, i.UpdatedAt = i.OpenedAt.UTC(), i.UpdatedAt.UTC()
	for _, t := range []**time.Time{&i.AcknowledgedAt, &i.ResolvedAt} {
		if *t != nil {
			u := (**t).UTC()
			*t = &u
		}
	}
	return i
}

// Get returns one incident.
func Get(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Incident, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM incidents WHERE id = $1`, id))
}

func getForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Incident, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM incidents WHERE id = $1 FOR UPDATE`, id))
}

// Filter narrows List. Status "active" means open or acknowledged.
type Filter struct {
	Status   *string
	Severity *string
	DeviceID *uuid.UUID
	CheckID  *uuid.UUID
	Before   *uuid.UUID // cursor: incidents with a smaller (older) ID
	Limit    int
}

// List returns incidents, newest first.
func List(ctx context.Context, tx pgx.Tx, f Filter) ([]Incident, error) {
	rows, err := tx.Query(ctx, `SELECT `+cols+` FROM incidents
		 WHERE ($1::text IS NULL OR status = $1 OR ($1 = 'active' AND status <> 'resolved'))
		   AND ($2::text IS NULL OR severity = $2)
		   AND ($3::uuid IS NULL OR device_id = $3)
		   AND ($4::uuid IS NULL OR check_id = $4)
		   AND ($5::uuid IS NULL OR id < $5)
		 ORDER BY id DESC LIMIT $6`, f.Status, f.Severity, f.DeviceID, f.CheckID, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Incident, error) { return scan(r) })
}

// Comment is a note on an incident.
type Comment struct {
	ID               uuid.UUID  `json:"id"`
	AuthorKind       string     `json:"author_kind"`
	AuthorUserID     *uuid.UUID `json:"author_user_id"`
	AuthorExternalID *string    `json:"author_external_id"`
	Body             string     `json:"body"`
	CreatedAt        time.Time  `json:"created_at"`
}

// Comments lists an incident's comments, oldest first.
func Comments(ctx context.Context, tx pgx.Tx, incident uuid.UUID) ([]Comment, error) {
	rows, err := tx.Query(ctx, `SELECT id, author_kind, author_user_id, author_external_id, body, created_at
		FROM incident_comments WHERE incident_id = $1 ORDER BY created_at, id`, incident)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Comment, error) {
		var c Comment
		err := r.Scan(&c.ID, &c.AuthorKind, &c.AuthorUserID, &c.AuthorExternalID, &c.Body, &c.CreatedAt)
		c.CreatedAt = c.CreatedAt.UTC()
		return c, err
	})
}
