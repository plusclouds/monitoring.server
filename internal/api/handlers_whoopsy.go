package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/threshold"
)

// Whoopsy! (premium band alerting) is turned on and off per check through
// its own endpoints, never through a check update.

func (s *Server) GetWhoopsy(ctx context.Context, req gen.GetWhoopsyRequestObject) (gen.GetWhoopsyResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var st gen.WhoopsyStatus
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err := inventory.GetCheck(ctx, tx, req.CheckId)
		if err != nil {
			return err
		}
		st, err = whoopsyStatus(ctx, tx, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.GetWhoopsy200JSONResponse(st), nil
}

func (s *Server) EnableWhoopsy(ctx context.Context, req gen.EnableWhoopsyRequestObject) (gen.EnableWhoopsyResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	w, err := via[threshold.Whoopsy](req.Body)
	if err != nil {
		return nil, err
	}
	var st gen.WhoopsyStatus
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err := inventory.SetWhoopsy(ctx, tx, p.Actor, req.CheckId, &w)
		if err != nil {
			return err
		}
		st, err = whoopsyStatus(ctx, tx, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.EnableWhoopsy200JSONResponse(st), nil
}

func (s *Server) DisableWhoopsy(ctx context.Context, req gen.DisableWhoopsyRequestObject) (gen.DisableWhoopsyResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		_, err := inventory.SetWhoopsy(ctx, tx, p.Actor, req.CheckId, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.DisableWhoopsy204Response{}, nil
}

// ResetWhoopsy forgets the learned band: the window fills again from the
// next results, so the current level becomes the new normal. An open
// Whoopsy! incident resolves with the next result.
func (s *Server) ResetWhoopsy(ctx context.Context, req gen.ResetWhoopsyRequestObject) (gen.ResetWhoopsyResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	var st gen.WhoopsyStatus
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err := inventory.GetCheck(ctx, tx, req.CheckId)
		if err != nil {
			return err
		}
		if c.Whoopsy == nil {
			return errs.Conflictf("whoopsy-off", "Whoopsy! is off for this check")
		}
		if _, err := tx.Exec(ctx, `UPDATE check_state SET machine = machine - 'whoopsy', version = version + 1, updated_at = now()
			WHERE check_id = $1`, c.ID); err != nil {
			return err
		}
		if err := audit.Write(ctx, tx, p.Actor.Event(t.ID, "check.whoopsy.reset", "check", c.ID.String(), nil, nil)); err != nil {
			return err
		}
		st, err = whoopsyStatus(ctx, tx, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.ResetWhoopsy200JSONResponse(st), nil
}

// whoopsyStatus combines a check's settings with the band the engine kept
// in its state.
func whoopsyStatus(ctx context.Context, tx pgx.Tx, c inventory.Check) (gen.WhoopsyStatus, error) {
	st := gen.WhoopsyStatus{CheckId: c.ID, Enabled: c.Whoopsy != nil}
	if c.Whoopsy == nil {
		return st, nil
	}
	settings, err := via[gen.Whoopsy](c.Whoopsy)
	if err != nil {
		return st, err
	}
	st.Settings = &settings
	var m engine.State
	err = tx.QueryRow(ctx, `SELECT machine FROM check_state WHERE check_id = $1`, c.ID).Scan(&m)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && m.Whoopsy == nil {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	ws := m.Whoopsy
	band := map[string]any{"points": len(ws.Values), "consecutive_hits": ws.Hits, "alerting": ws.Level != "",
		"mean": nil, "stddev": nil, "lower": nil, "upper": nil, "last_value": nil}
	if b := ws.Band; b != nil {
		band["mean"], band["stddev"], band["lower"], band["upper"] = finite(b.Mean), finite(b.StdDev), finite(b.Lower), finite(b.Upper)
	}
	if len(ws.Values) > 0 {
		band["last_value"] = finite(ws.Last)
	}
	b, _ := json.Marshal(map[string]any{"band": band})
	var out gen.WhoopsyStatus
	if err := json.Unmarshal(b, &out); err != nil {
		return st, err
	}
	st.Band = out.Band
	return st, nil
}

// MaxBandSpan and MaxBandPoints bound a band request.
const (
	maxBandSpan   = 7 * 24 * time.Hour
	maxBandPoints = 20000
)

func (s *Server) GetWhoopsyBand(ctx context.Context, req gen.GetWhoopsyBandRequestObject) (gen.GetWhoopsyBandResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	to := time.Now()
	if req.Params.To != nil {
		to = *req.Params.To
	}
	from := to.Add(-time.Hour)
	if req.Params.From != nil {
		from = *req.Params.From
	}
	if !from.Before(to) {
		return nil, errs.Invalidf("from", "must be before to")
	}
	if to.Sub(from) > maxBandSpan {
		return nil, errs.Invalidf("from", "at most 7 days per request")
	}
	out := gen.GetWhoopsyBand200JSONResponse{CheckId: req.CheckId, From: from.UTC(), To: to.UTC()}
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if _, err := inventory.GetCheck(ctx, tx, req.CheckId); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT ts, value, mean, stddev, lower_bound, upper_bound, outside, hits, alerting
			FROM whoopsy_band WHERE check_id = $1 AND ts >= $2 AND ts < $3 ORDER BY ts LIMIT $4`,
			req.CheckId, from, to, maxBandPoints+1)
		if err != nil {
			return err
		}
		type point = struct {
			Alerting bool      `json:"alerting"`
			Hits     int       `json:"hits"`
			Lower    *float64  `json:"lower"`
			Mean     *float64  `json:"mean"`
			Outside  bool      `json:"outside"`
			Stddev   *float64  `json:"stddev"`
			T        time.Time `json:"t"`
			Upper    *float64  `json:"upper"`
			Value    float64   `json:"value"`
		}
		pts := []point{}
		var p point
		if _, err := pgx.ForEachRow(rows, []any{&p.T, &p.Value, &p.Mean, &p.Stddev, &p.Lower, &p.Upper, &p.Outside, &p.Hits, &p.Alerting},
			func() error {
				q := p
				q.T = q.T.UTC()
				pts = append(pts, q)
				p = point{}
				return nil
			}); err != nil {
			return err
		}
		if len(pts) > maxBandPoints {
			return errs.Invalidf("from", "the window holds more than %d results; narrow it", maxBandPoints)
		}
		conv, err := via[gen.GetWhoopsyBand200JSONResponse](map[string]any{"points": pts})
		out.Points = conv.Points
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
