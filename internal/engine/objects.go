package engine

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/incident"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// goneRetention is how long an object that stopped being reported keeps its
// row, so a port that comes back finds its history.
const goneRetention = 30 * 24 * time.Hour

type objectRow struct {
	machine  State
	incident *uuid.UUID
	gone     bool
}

// applyObjects runs the state machine of every object of a successful
// collector run (F05: per-object state and incidents), in the run's
// transaction. Objects missing from the run are marked gone and their
// incidents resolve.
func (e *Engine) applyObjects(ctx context.Context, tx pgx.Tx, r runner.Result, device uuid.UUID,
	devices map[string]uuid.UUID, cfg Config, m plugin.Manifest) error {
	rows, err := tx.Query(ctx, `SELECT object_key, machine, incident_id, gone_at IS NOT NULL
		FROM check_objects WHERE check_id = $1 FOR UPDATE`, r.CheckID)
	if err != nil {
		return err
	}
	prev := map[string]objectRow{}
	var key string
	var machine []byte
	var row objectRow
	if _, err := pgx.ForEachRow(rows, []any{&key, &machine, &row.incident, &row.gone}, func() error {
		o := objectRow{gone: row.gone}
		if row.incident != nil { // pgx may reuse the pointer for the next row
			id := *row.incident
			o.incident, row.incident = &id, nil
		}
		if err := json.Unmarshal(machine, &o.machine); err != nil {
			return err
		}
		prev[key] = o
		return nil
	}); err != nil {
		return err
	}

	allRules := cfg.Rules
	batch := &pgx.Batch{}
	seen := make(map[string]bool, len(r.Objects))
	for i := range r.Objects {
		obj := &r.Objects[i]
		seen[obj.Key] = true
		p := prev[obj.Key]
		if p.gone {
			p.machine = State{} // back after being gone: start over
		}
		cfg.Rules = cfg.Rules[:0:0]
		for _, rule := range allRules {
			if rule.AppliesTo(obj.Key, obj.Name) {
				cfg.Rules = append(cfg.Rules, rule)
			}
		}
		out := Evaluate(cfg, p.machine, Input{Status: obj.Status, Output: obj.Output, Metrics: obj.Metrics, Time: r.Time})
		if out.Action != ActionNone {
			out.Summary = obj.Name + ": " + out.Summary
		}
		objDevice := device
		if d, ok := devices[obj.Key]; ok {
			objDevice = d // a VM's incidents belong to the VM
		}
		inc, err := e.act(ctx, tx, r, objDevice, obj, false, p.incident, out)
		if err != nil {
			return err
		}
		if out.Action == ActionResolve {
			inc = nil
		}
		batch.Queue(`
			INSERT INTO check_objects (check_id, object_key, tenant_id, name, labels, phase, status, since, machine,
			                           last_status, last_output, last_metrics, incident_id, last_seen_at, gone_at, device_id,
			                           billing_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NULL, $15, $16)
			ON CONFLICT (check_id, object_key) DO UPDATE SET
			       name = excluded.name, labels = excluded.labels, phase = excluded.phase, status = excluded.status,
			       since = excluded.since, machine = excluded.machine, last_status = excluded.last_status,
			       last_output = excluded.last_output, last_metrics = excluded.last_metrics,
			       incident_id = excluded.incident_id, last_seen_at = excluded.last_seen_at, gone_at = NULL,
			       device_id = excluded.device_id, billing_key = excluded.billing_key`,
			r.CheckID, obj.Key, r.TenantID, obj.Name, nonNilLabels(obj.Labels), out.State.Phase, out.State.Status,
			out.State.Since, out.State, obj.Status.String(), obj.Output, metricMap(cfg.Metrics, obj.Metrics), inc, r.Time,
			nullDevice(devices, obj.Key), nullString(m.BillingKey(obj.Key)))
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}

	// Objects the device no longer reports (a removed line card, a port
	// filtered out): resolve their incidents and keep the row for a while.
	for key, p := range prev {
		if seen[key] || p.gone {
			continue
		}
		if p.incident != nil {
			if _, _, err := incident.Resolve(ctx, tx, *p.incident, "object-gone", nil); err != nil {
				return err
			}
			e.incidents.WithLabelValues("resolve").Inc()
		}
		if _, err := tx.Exec(ctx, `UPDATE check_objects SET gone_at = $3, incident_id = NULL,
			phase = 'OK', status = 'UNKNOWN', last_output = 'no longer reported by the device'
			WHERE check_id = $1 AND object_key = $2`, r.CheckID, key, r.Time); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM check_objects WHERE check_id = $1 AND gone_at < $2`,
		r.CheckID, r.Time.Add(-goneRetention)); err != nil {
		return err
	}
	// Billable objects (pool hosts) start and end their periods (F13).
	if len(m.BillableObjects) > 0 {
		_, err = tx.Exec(ctx, `SELECT usage_sync_check_objects($1)`, r.CheckID)
	}
	return err
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func metricMap(names []string, values []float64) map[string]*float64 {
	out := map[string]*float64{}
	for i, name := range names {
		if i < len(values) && !math.IsNaN(values[i]) && !math.IsInf(values[i], 0) {
			v := values[i]
			out[name] = &v
		}
	}
	return out
}

func nonNilLabels(l map[string]string) map[string]string {
	if l == nil {
		return map[string]string{}
	}
	return l
}

func nullDevice(devices map[string]uuid.UUID, key string) *uuid.UUID {
	if d, ok := devices[key]; ok {
		return &d
	}
	return nil
}
