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
	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/incident"
	"github.com/plusclouds/monitoring.server/internal/inventory"
)

// via converts between types with the same JSON shape: domain objects whose
// JSON is their public API representation, and the generated API types.
func via[T any](v any) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(b, &out)
}

func incidentActor(p *Principal) incident.Actor {
	a := incident.Actor{Kind: p.Actor.Kind, UserID: p.Actor.UserID}
	if p.Actor.ExternalID != "" {
		ext := p.Actor.ExternalID
		a.ExternalID = &ext
	}
	return a
}

func (s *Server) GetCheckState(ctx context.Context, req gen.GetCheckStateRequestObject) (gen.GetCheckStateResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var st gen.CheckState
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if _, err := inventory.GetCheck(ctx, tx, req.CheckId); err != nil {
			return err
		}
		var m engine.State
		var metrics map[string]float64
		err := tx.QueryRow(ctx, `SELECT check_id, phase, status, since, last_result_at, last_status, last_output,
			       last_metrics, machine, incident_id, updated_at FROM check_state WHERE check_id = $1`, req.CheckId).
			Scan(&st.CheckId, &st.Phase, &st.Status, &st.Since, &st.LastResultAt, &st.LastStatus, &st.LastOutput,
				&metrics, &m, &st.IncidentId, &st.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(http.StatusNotFound, "no-state", "No state yet", "The check has not produced a result yet.")
		}
		st.LastMetrics = metrics
		if st.LastMetrics == nil {
			st.LastMetrics = map[string]float64{}
		}
		st.Flapping, st.ConsecutiveBad, st.ConsecutiveGood = m.Flapping, m.ConsecutiveBad, m.ConsecutiveGood
		st.Since, st.UpdatedAt = st.Since.UTC(), st.UpdatedAt.UTC()
		if st.LastResultAt != nil {
			u := st.LastResultAt.UTC()
			st.LastResultAt = &u
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.GetCheckState200JSONResponse(st), nil
}

// ListCheckObjects lists a collector check's objects with their state.
func (s *Server) ListCheckObjects(ctx context.Context, req gen.ListCheckObjectsRequestObject) (gen.ListCheckObjectsResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var status *string
	if req.Params.Status != nil {
		v := string(*req.Params.Status)
		status = &v
	}
	includeGone := req.Params.IncludeGone == nil || *req.Params.IncludeGone
	out := gen.ListCheckObjects200JSONResponse{Items: []gen.CheckObject{}}
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if _, err := inventory.GetCheck(ctx, tx, req.CheckId); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT object_key, name, labels, phase, status, since, last_status, last_output, last_metrics,
			       incident_id, device_id, first_seen_at, last_seen_at, gone_at
			  FROM check_objects
			 WHERE check_id = $1 AND ($2::text IS NULL OR status = $2) AND ($3 OR gone_at IS NULL)
			 ORDER BY object_key`, req.CheckId, status, includeGone)
		if err != nil {
			return err
		}
		out.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (gen.CheckObject, error) {
			var o gen.CheckObject
			var metrics map[string]float64
			err := r.Scan(&o.Key, &o.Name, &o.Labels, &o.Phase, &o.Status, &o.Since, &o.LastStatus, &o.LastOutput,
				&metrics, &o.IncidentId, &o.DeviceId, &o.FirstSeenAt, &o.LastSeenAt, &o.GoneAt)
			o.LastMetrics = metrics
			if o.LastMetrics == nil {
				o.LastMetrics = map[string]float64{}
			}
			if o.Labels == nil {
				o.Labels = map[string]string{}
			}
			o.Since, o.FirstSeenAt, o.LastSeenAt, o.GoneAt = o.Since.UTC(), o.FirstSeenAt.UTC(), o.LastSeenAt.UTC(), utcPtr(o.GoneAt)
			return o, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) RunCheckNow(ctx context.Context, req gen.RunCheckNowRequestObject) (gen.RunCheckNowResponseObject, error) {
	p, t, err := tenantScope(ctx, roleOperator, true)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err := inventory.GetCheck(ctx, tx, req.CheckId)
		if err != nil {
			return err
		}
		if !c.Enabled {
			return problem(http.StatusConflict, "check-disabled", "Check disabled", "Enable the check to run it.")
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify('run_now', $1)`, c.ID.String()); err != nil {
			return err
		}
		return audit.Write(ctx, tx, p.Actor.Event(t.ID, "check.run_now", "check", c.ID.String(), nil, nil))
	})
	if err != nil {
		return nil, err
	}
	return gen.RunCheckNow202Response{}, nil
}

func (s *Server) ListIncidents(ctx context.Context, req gen.ListIncidentsRequestObject) (gen.ListIncidentsResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	q := req.Params
	n := 100
	if q.Limit != nil {
		n = *q.Limit
	}
	f := incident.Filter{DeviceID: q.DeviceId, CheckID: q.CheckId, Suppressed: q.Suppressed, ObjectKey: q.ObjectKey, Limit: n + 1}
	if q.Status != nil {
		v := string(*q.Status)
		f.Status = &v
	}
	if q.Severity != nil {
		v := string(*q.Severity)
		f.Severity = &v
	}
	if q.Cursor != nil {
		id, err := decodeCursorID(*q.Cursor)
		if err != nil {
			return nil, err
		}
		f.Before = &id
	}
	var items []incident.Incident
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		items, err = incident.List(ctx, tx, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	items, next := trim(items, n, func(x incident.Incident) uuid.UUID { return x.ID })
	out := gen.ListIncidents200JSONResponse{Items: make([]gen.Incident, len(items)), NextCursor: next}
	for i, x := range items {
		if out.Items[i], err = via[gen.Incident](x.UTC()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) GetIncident(ctx context.Context, req gen.GetIncidentRequestObject) (gen.GetIncidentResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var inc incident.Incident
	var comments []incident.Comment
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if inc, err = incident.Get(ctx, tx, req.IncidentId); err != nil {
			return err
		}
		comments, err = incident.Comments(ctx, tx, inc.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := via[gen.GetIncident200JSONResponse](struct {
		incident.Incident
		Comments []incident.Comment `json:"comments"`
	}{inc.UTC(), nonNilSlice(comments)})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// changeIncident runs an operator action on an incident with its audit event.
func (s *Server) changeIncident(ctx context.Context, id uuid.UUID, action string,
	fn func(pgx.Tx, *Principal) (incident.Incident, bool, error)) (gen.Incident, error) {
	p, t, err := tenantScope(ctx, roleOperator, true)
	if err != nil {
		return gen.Incident{}, err
	}
	var inc incident.Incident
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		before, err := incident.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		var changed bool
		if inc, changed, err = fn(tx, p); err != nil || !changed {
			return err
		}
		return audit.Write(ctx, tx, p.Actor.Event(t.ID, action, "incident", id.String(),
			map[string]any{"status": before.Status}, map[string]any{"status": inc.Status}))
	})
	if err != nil {
		return gen.Incident{}, err
	}
	return via[gen.Incident](inc.UTC())
}

func (s *Server) AcknowledgeIncident(ctx context.Context, req gen.AcknowledgeIncidentRequestObject) (gen.AcknowledgeIncidentResponseObject, error) {
	out, err := s.changeIncident(ctx, req.IncidentId, "incident.acknowledge", func(tx pgx.Tx, p *Principal) (incident.Incident, bool, error) {
		return incident.Acknowledge(ctx, tx, req.IncidentId, incidentActor(p))
	})
	if err != nil {
		return nil, err
	}
	return gen.AcknowledgeIncident200JSONResponse(out), nil
}

// ResolveIncident resolves by hand and starts the check's state over, so a
// problem that persists opens a new incident (F05).
func (s *Server) ResolveIncident(ctx context.Context, req gen.ResolveIncidentRequestObject) (gen.ResolveIncidentResponseObject, error) {
	out, err := s.changeIncident(ctx, req.IncidentId, "incident.resolve", func(tx pgx.Tx, p *Principal) (incident.Incident, bool, error) {
		a := incidentActor(p)
		inc, changed, err := incident.Resolve(ctx, tx, req.IncidentId, "manual", &a)
		if err != nil || !changed || inc.CheckID == nil {
			return inc, changed, err
		}
		now := time.Now().UTC()
		reset := engine.State{Phase: engine.PhaseOK, Status: "OK", Since: now, AvailabilitySince: now}
		_, err = tx.Exec(ctx, `UPDATE check_state SET phase = 'OK', status = 'OK', since = now(), machine = $2,
			availability_since = now(),
			incident_id = NULL, version = version + 1, updated_at = now() WHERE check_id = $1`, *inc.CheckID, reset)
		return inc, changed, err
	})
	if err != nil {
		return nil, err
	}
	return gen.ResolveIncident200JSONResponse(out), nil
}

func (s *Server) CommentIncident(ctx context.Context, req gen.CommentIncidentRequestObject) (gen.CommentIncidentResponseObject, error) {
	p, t, err := tenantScope(ctx, roleOperator, true)
	if err != nil {
		return nil, err
	}
	var c incident.Comment
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if c, err = incident.AddComment(ctx, tx, req.IncidentId, incidentActor(p), req.Body.Body); err != nil {
			return err
		}
		return audit.Write(ctx, tx, p.Actor.Event(t.ID, "incident.comment", "incident", req.IncidentId.String(),
			nil, map[string]any{"comment_id": c.ID}))
	})
	if err != nil {
		return nil, err
	}
	out, err := via[gen.IncidentComment](c)
	if err != nil {
		return nil, err
	}
	return gen.CommentIncident201JSONResponse(out), nil
}
