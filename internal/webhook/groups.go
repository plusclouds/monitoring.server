package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/incident"
)

// groupKey is the values of a route's group_by fields for an event.
func groupKey(env *incident.Envelope, fields []string) map[string]any {
	obj, _ := env.Data.Object.(map[string]any)
	d, c := env.Data.Device, env.Data.Check
	key := map[string]any{}
	for _, f := range fields {
		var v any
		switch f {
		case "root_device_id":
			v = obj["root_device_id"]
			if v == nil && d != nil {
				v = d.ID
			}
		case "device_id":
			if d != nil {
				v = d.ID
			}
		case "site_id":
			if d != nil {
				v = d.SiteID
			}
		case "device_type":
			if d != nil {
				v = d.Type
			}
		case "severity":
			v = obj["severity"]
		case "check_id":
			if c != nil {
				v = c.ID
			}
		case "plugin":
			if c != nil {
				v = c.Plugin
			}
		}
		key[f] = v
	}
	return key
}

// addToGroup puts an event into the open group of its route, event type
// and key, opening one that flushes after the route's group_wait.
func (n *Notifier) addToGroup(ctx context.Context, tx pgx.Tx, e unrouted, env *incident.Envelope, r Route) error {
	key, err := json.Marshal(groupKey(env, r.GroupBy))
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	var gid uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO alert_groups (id, tenant_id, route_id, endpoint_id, event_type, group_key, flush_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))
		ON CONFLICT (route_id, event_type, group_key) WHERE flushed_at IS NULL DO NOTHING RETURNING id`,
		id, e.tenant, r.ID, r.EndpointID, e.typ, json.RawMessage(key), r.GroupWait.Seconds()).Scan(&gid)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id FROM alert_groups
			WHERE route_id = $1 AND event_type = $2 AND group_key = $3 AND flushed_at IS NULL`,
			r.ID, e.typ, json.RawMessage(key)).Scan(&gid)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO alert_group_events (group_id, event_id, event_seq) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, gid, e.id, e.seq)
	return err
}

// flushGroups sends every group whose wait has passed: a single event as
// itself, several as one event carrying all of them in data.objects.
func (n *Notifier) flushGroups(ctx context.Context) error {
	return pgx.BeginFunc(ctx, n.o.System, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, route_id, event_type, group_key FROM alert_groups
			WHERE flushed_at IS NULL AND flush_at <= now() ORDER BY flush_at LIMIT 100 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		type group struct {
			id, tenant, route uuid.UUID
			typ               string
			key               json.RawMessage
		}
		groups, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (group, error) {
			var g group
			err := r.Scan(&g.id, &g.tenant, &g.route, &g.typ, &g.key)
			return g, err
		})
		if err != nil {
			return err
		}
		for _, g := range groups {
			r, err := GetRoute(ctx, tx, g.route)
			if err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT e.id, e.seq, e.subject, e.envelope FROM alert_group_events m
				JOIN events e ON e.id = m.event_id WHERE m.group_id = $1 ORDER BY e.seq`, g.id)
			if err != nil {
				return err
			}
			type member struct {
				id      uuid.UUID
				seq     int64
				subject *string
				env     incident.Envelope
			}
			members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (member, error) {
				var m member
				var raw []byte
				if err := row.Scan(&m.id, &m.seq, &m.subject, &raw); err != nil {
					return m, err
				}
				return m, json.Unmarshal(raw, &m.env)
			})
			if err != nil {
				return err
			}
			switch len(members) {
			case 0:
			case 1:
				m := members[0]
				if err := n.enqueue(ctx, tx, g.tenant, m.id, m.seq, g.typ, deref(m.subject), r); err != nil {
					return err
				}
			default:
				envs := make([]incident.Envelope, len(members))
				for i, m := range members {
					envs[i] = m.env
				}
				id, seq, subject, err := writeGroupEvent(ctx, tx, g.tenant, g.typ, g.id, g.key, envs)
				if err != nil {
					return err
				}
				if err := n.enqueue(ctx, tx, g.tenant, id, seq, g.typ, subject, r); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE alert_groups SET flushed_at = now() WHERE id = $1`, g.id); err != nil {
				return err
			}
		}
		return nil
	})
}

// GroupObject is one incident of a grouped event: what data.object,
// data.device and data.check say in a single event.
type GroupObject struct {
	Object any                 `json:"object"`
	Device *incident.DeviceRef `json:"device"`
	Check  *incident.CheckRef  `json:"check"`
}

// writeGroupEvent stores the event of a flushed group, already routed.
// Its data carries the incidents in objects and the group's key in group;
// data.object, data.device and data.check are null.
func writeGroupEvent(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, typ string, group uuid.UUID,
	key json.RawMessage, members []incident.Envelope) (uuid.UUID, int64, string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return id, 0, "", err
	}
	first := members[0]
	objects := make([]GroupObject, len(members))
	for i, m := range members {
		objects[i] = GroupObject{Object: m.Data.Object, Device: m.Data.Device, Check: m.Data.Check}
	}
	now := time.Now().UTC()
	subject := "group:" + group.String()
	env := map[string]any{
		"specversion": "1.0", "id": id, "source": first.Source, "type": typ, "time": now,
		"datacontenttype": "application/json", "subject": subject,
		"data": map[string]any{
			"account_id": first.Data.AccountID, "object_type": first.Data.ObjectType,
			"object": nil, "device": nil, "check": nil, "route": nil, "actor": nil,
			"objects": objects,
			"group":   map[string]any{"id": group, "key": key, "count": len(members)},
		},
	}
	var seq int64
	err = tx.QueryRow(ctx, `INSERT INTO events (id, tenant_id, type, subject, time, envelope, routed_at)
		VALUES ($1, $2, $3, $4, $5, $6, now()) RETURNING seq`, id, tenant, typ, subject, now, env).Scan(&seq)
	return id, seq, subject, err
}

// repeat re-sends open, unacknowledged, unsuppressed incidents to routes
// with repeat_interval, as monitoring.incident.renotify (F06).
func (n *Notifier) repeat(ctx context.Context) error {
	return pgx.BeginFunc(ctx, n.o.System, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM route_notifications rn USING incidents i
			WHERE i.id = rn.incident_id AND i.status = 'resolved'`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT rn.route_id, rn.incident_id FROM route_notifications rn
			  JOIN alert_routes r ON r.id = rn.route_id
			  JOIN incidents i ON i.id = rn.incident_id
			 WHERE r.enabled AND r.repeat_interval_seconds IS NOT NULL AND i.status = 'open' AND NOT i.suppressed
			   AND rn.last_sent_at <= now() - make_interval(secs => r.repeat_interval_seconds)
			 ORDER BY rn.last_sent_at LIMIT 100 FOR UPDATE OF rn SKIP LOCKED`)
		if err != nil {
			return err
		}
		type due struct{ route, incident uuid.UUID }
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
			var d due
			err := r.Scan(&d.route, &d.incident)
			return d, err
		})
		if err != nil {
			return err
		}
		for _, d := range list {
			r, err := GetRoute(ctx, tx, d.route)
			if err != nil {
				return err
			}
			inc, err := incident.Get(ctx, tx, d.incident)
			if err != nil {
				return err
			}
			id, seq, err := incident.EmitRouted(ctx, tx, incident.EventRenotify, inc)
			if err != nil {
				return err
			}
			if err := n.enqueue(ctx, tx, inc.TenantID, id, seq, incident.EventRenotify, inc.ID.String(), r); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE route_notifications SET last_sent_at = now()
				WHERE route_id = $1 AND incident_id = $2`, d.route, d.incident); err != nil {
				return err
			}
		}
		return nil
	})
}
