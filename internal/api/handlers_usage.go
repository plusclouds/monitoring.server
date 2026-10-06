package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/usage"
)

// usageRange validates a usage range against the closed hours.
func (s *Server) usageRange(ctx context.Context, from, to time.Time) (time.Time, time.Time, error) {
	from, to = from.UTC(), to.UTC()
	until, err := usage.ClosedUntil(ctx, s.db)
	if err != nil {
		return from, to, err
	}
	err = usage.CheckRange(from, to, until)
	var open *usage.ErrOpen
	if errors.As(err, &open) {
		return from, to, problem(http.StatusUnprocessableEntity, "usage-hour-open", "Hour not closed",
			"Hours from "+open.ClosedUntil.Format(time.RFC3339)+" on are not closed yet; read up to that time.")
	}
	return from, to, err
}

func toAPIUsage(h usage.Hour) (gen.UsageHour, error) {
	out, err := via[gen.UsageHour](map[string]any{"breakdown": h.Breakdown})
	if err != nil {
		return out, err
	}
	out.AccountId, out.PeriodStart, out.PeriodEnd = h.AccountID, h.PeriodStart, h.PeriodStart.Add(time.Hour)
	out.BillableCheckSeconds, out.DeviceSeconds, out.Revision = h.BillableCheckSeconds, h.DeviceSeconds, h.Revision
	return out, nil
}

func encodeUsageCursor(c usage.Cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeUsageCursor(s string) (*usage.Cursor, error) {
	var c usage.Cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		return nil, problem(http.StatusBadRequest, "invalid-cursor", "Invalid cursor", "Use next_cursor from the previous page.")
	}
	return &c, nil
}

func (s *Server) ListTenantUsage(ctx context.Context, req gen.ListTenantUsageRequestObject) (gen.ListTenantUsageResponseObject, error) {
	if _, err := platformScope(ctx); err != nil {
		return nil, err
	}
	p := req.Params
	from, to, err := s.usageRange(ctx, p.From, p.To)
	if err != nil {
		return nil, err
	}
	limit := 100
	if p.Limit != nil {
		limit = *p.Limit
	}
	var after *usage.Cursor
	if p.Cursor != nil {
		if after, err = decodeUsageCursor(*p.Cursor); err != nil {
			return nil, err
		}
	}
	hours, err := usage.TenantHours(ctx, s.db, from, to, after, limit+1)
	if err != nil {
		return nil, err
	}
	out := gen.ListTenantUsage200JSONResponse{Items: []gen.UsageHour{}}
	for i, h := range hours {
		if i == limit {
			last := hours[i-1]
			c := encodeUsageCursor(usage.Cursor{Period: last.PeriodStart, Account: *last.AccountID})
			out.NextCursor = &c
			break
		}
		item, err := toAPIUsage(h)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func (s *Server) ListUsageHours(ctx context.Context, req gen.ListUsageHoursRequestObject) (gen.ListUsageHoursResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	from, to, err := s.usageRange(ctx, req.Params.From, req.Params.To)
	if err != nil {
		return nil, err
	}
	var hours []usage.Hour
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		hours, err = usage.TenantOwnHours(ctx, tx, from, to)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := gen.ListUsageHours200JSONResponse{Items: make([]gen.UsageHour, len(hours))}
	for i, h := range hours {
		if out.Items[i], err = toAPIUsage(h); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) GetCurrentUsage(ctx context.Context, _ gen.GetCurrentUsageRequestObject) (gen.GetCurrentUsageResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var cur usage.Current
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err = usage.CurrentUsage(ctx, tx, time.Now())
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := via[gen.GetCurrentUsage200JSONResponse](map[string]any{"by_plugin": cur.ByPlugin})
	if err != nil {
		return nil, err
	}
	out.Checks, out.WeightedChecks = cur.Checks, cur.WeightedChecks
	return out, nil
}

func (s *Server) GetUsageWeights(ctx context.Context, _ gen.GetUsageWeightsRequestObject) (gen.GetUsageWeightsResponseObject, error) {
	weights, def, err := usage.Weights(ctx, s.db, time.Now())
	if err != nil {
		return nil, err
	}
	mult, ok := weights[usage.WhoopsyMultiplier]
	if !ok {
		mult = 5
	}
	delete(weights, usage.WhoopsyMultiplier)
	return gen.GetUsageWeights200JSONResponse{DefaultWeight: def, WhoopsyMultiplier: mult, Weights: weights}, nil
}
