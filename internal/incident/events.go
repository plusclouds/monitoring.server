package incident

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/extref"
)

// Event types (ADR-0009).
const (
	EventOpened       = "monitoring.incident.opened"
	EventUpdated      = "monitoring.incident.updated"
	EventAcknowledged = "monitoring.incident.acknowledged"
	EventResolved     = "monitoring.incident.resolved"
	EventCommented    = "monitoring.incident.commented"
	// EventRenotify repeats an open, unacknowledged incident for a route
	// with repeat_interval (F06). It goes to that route only.
	EventRenotify = "monitoring.incident.renotify"
)

// ObjectType is data.object_type of incident events, in PlusClouds' style.
const ObjectType = `Monitoring\Incidents`

// Envelope is a CloudEvents 1.0 event, the shape PlusClouds' own event
// pushers use (ADR-0009).
type Envelope struct {
	SpecVersion     string    `json:"specversion"`
	ID              uuid.UUID `json:"id"`
	Source          string    `json:"source"`
	Type            string    `json:"type"`
	Time            time.Time `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	Subject         string    `json:"subject"`
	Data            Data      `json:"data"`
}

// Data is the event's payload. Route is filled per delivery by the
// notifier (F06).
type Data struct {
	AccountID  *string         `json:"account_id"`
	ObjectType string          `json:"object_type"`
	Object     any             `json:"object"`
	Device     *DeviceRef      `json:"device"`
	Check      *CheckRef       `json:"check"`
	Route      json.RawMessage `json:"route"`
	Actor      *Actor          `json:"actor"`
}

// DeviceRef is the device an event is about, with its containment path so
// receivers can map it to their own objects without a lookup (ADR-0012).
type DeviceRef struct {
	ID       uuid.UUID         `json:"id"`
	Name     string            `json:"name"`
	Address  string            `json:"address"`
	Type     string            `json:"type"`
	SiteID   *uuid.UUID        `json:"site_id"`
	Tags     map[string]string `json:"tags"`
	External *extref.Ref       `json:"external"`
	Path     []PathItem        `json:"path"` // containers, outermost first
}

// PathItem is one container of a device.
type PathItem struct {
	ID       uuid.UUID   `json:"id"`
	Name     string      `json:"name"`
	External *extref.Ref `json:"external"`
}

// CheckRef is the check an event is about.
type CheckRef struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Plugin     string    `json:"plugin"`
	LastOutput *string   `json:"last_output"`
	RuleID     *string   `json:"rule_id"`
	RuleName   *string   `json:"rule_name"`
	RunbookURL *string   `json:"runbook_url"`
}

// Actor is who acknowledged, resolved or commented.
type Actor struct {
	Kind       string     `json:"kind"`
	UserID     *uuid.UUID `json:"user_id"`
	ExternalID *string    `json:"external_id"`
}

// emit writes an event about an incident in tx, for the router.
func emit(ctx context.Context, tx pgx.Tx, typ string, inc Incident, actor *Actor, extra map[string]any) error {
	_, _, err := write(ctx, tx, typ, inc, actor, extra, false)
	return err
}

// EmitRouted writes an event that is already routed: the caller creates its
// deliveries. It returns the event's ID and sequence number.
func EmitRouted(ctx context.Context, tx pgx.Tx, typ string, inc Incident) (uuid.UUID, int64, error) {
	return write(ctx, tx, typ, inc, nil, nil, true)
}

func write(ctx context.Context, tx pgx.Tx, typ string, inc Incident, actor *Actor, extra map[string]any,
	routed bool) (uuid.UUID, int64, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return id, 0, err
	}
	var installation uuid.UUID
	var account *string
	if err := tx.QueryRow(ctx, `SELECT (SELECT id FROM installation),
		       (SELECT external_id FROM tenant_by_id($1))`, inc.TenantID).Scan(&installation, &account); err != nil {
		return id, 0, err
	}
	data := Data{AccountID: account, ObjectType: ObjectType, Actor: actor, Route: json.RawMessage("null")}
	obj := map[string]any{}
	b, _ := json.Marshal(inc.UTC())
	_ = json.Unmarshal(b, &obj)
	for k, v := range extra {
		obj[k] = v
	}
	data.Object = obj
	if data.Device, err = device(ctx, tx, inc.DeviceID); err != nil {
		return id, 0, err
	}
	if data.Check, err = check(ctx, tx, inc); err != nil {
		return id, 0, err
	}
	now := time.Now().UTC()
	env := Envelope{
		SpecVersion: "1.0", ID: id, Source: "/monitoring/" + installation.String() + "/" + inc.TenantID.String(),
		Type: typ, Time: now, DataContentType: "application/json", Subject: inc.ID.String(), Data: data,
	}
	var seq int64
	err = tx.QueryRow(ctx, `INSERT INTO events (id, tenant_id, type, subject, time, envelope, routed_at)
		VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $7 THEN now() END) RETURNING seq`,
		id, inc.TenantID, typ, env.Subject, now, env, routed).Scan(&seq)
	return id, seq, err
}

func device(ctx context.Context, tx pgx.Tx, id *uuid.UUID) (*DeviceRef, error) {
	if id == nil {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE up(id, depth) AS (
			SELECT $1::uuid, 0
			UNION ALL
			SELECT d.parent_id, up.depth + 1 FROM up JOIN devices d ON d.id = up.id
			 WHERE d.parent_id IS NOT NULL AND up.depth < 50
		)
		SELECT d.id, d.name, d.address, d.type, d.site_id, d.tags, d.external_source, d.external_type, d.external_id
		  FROM up JOIN devices d ON d.id = up.id ORDER BY up.depth`, *id)
	if err != nil {
		return nil, err
	}
	refs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeviceRef, error) {
		var d DeviceRef
		var src, typ, ext *string
		err := r.Scan(&d.ID, &d.Name, &d.Address, &d.Type, &d.SiteID, &d.Tags, &src, &typ, &ext)
		d.External = extref.FromColumns(src, typ, ext)
		return d, err
	})
	if err != nil || len(refs) == 0 {
		return nil, err
	}
	out := refs[0]
	out.Path = []PathItem{}
	for i := len(refs) - 1; i > 0; i-- {
		out.Path = append(out.Path, PathItem{ID: refs[i].ID, Name: refs[i].Name, External: refs[i].External})
	}
	return &out, nil
}

func check(ctx context.Context, tx pgx.Tx, inc Incident) (*CheckRef, error) {
	if inc.CheckID == nil {
		return nil, nil
	}
	c := &CheckRef{ID: *inc.CheckID, RuleID: inc.RuleID, RuleName: inc.RuleName}
	err := tx.QueryRow(ctx, `SELECT c.name, c.plugin, c.runbook_url, s.last_output
		FROM checks c LEFT JOIN check_state s ON s.check_id = c.id WHERE c.id = $1`, *inc.CheckID).
		Scan(&c.Name, &c.Plugin, &c.RunbookURL, &c.LastOutput)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}
