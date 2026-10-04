package api

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/store"
)

func selector(device, check *uuid.UUID, names *[]string, object *string) metrics.Selector {
	sel := metrics.Selector{DeviceID: device, CheckID: check, Object: object}
	if names != nil {
		sel.Names = *names
	}
	return sel
}

func (s *Server) ListMetricSeries(ctx context.Context, req gen.ListMetricSeriesRequestObject) (gen.ListMetricSeriesResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	q := req.Params
	var list []metrics.Series
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		list, err = metrics.ListSeries(ctx, tx, selector(q.DeviceId, q.CheckId, q.Name, q.Object))
		return err
	})
	if err != nil {
		return nil, err
	}
	out := gen.ListMetricSeries200JSONResponse{Items: make([]gen.MetricSeries, len(list))}
	for i, x := range list {
		out.Items[i] = gen.MetricSeries{Id: x.ID, DeviceId: x.DeviceID, CheckId: x.CheckID, Plugin: x.Plugin,
			Object: x.Object, Name: x.Name, Unit: x.Unit, Kind: gen.MetricSeriesKind(x.Kind), RetentionClass: x.Class}
	}
	return out, nil
}

func (s *Server) QueryMetrics(ctx context.Context, req gen.QueryMetricsRequestObject) (gen.QueryMetricsResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	p := req.Params
	q := metrics.Query{Selector: selector(p.DeviceId, p.CheckId, p.Name, p.Object), MaxPoints: s.maxPoints}
	if p.From != nil {
		q.From = *p.From
	}
	if p.To != nil {
		q.To = *p.To
	}
	if p.Step != nil {
		q.Step = time.Duration(*p.Step) * time.Second
	}
	if p.Agg != nil {
		q.Agg = string(*p.Agg)
	}
	if p.Resolution != nil {
		q.Resolution = string(*p.Resolution)
	}
	var res metrics.Result
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		res, err = metrics.Run(ctx, tx, q)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := gen.QueryMetrics200JSONResponse{Resolution: gen.MetricQueryResultResolution(res.Resolution),
		Step: int(res.Step / time.Second), Series: make([]gen.MetricQuerySeries, len(res.Series))}
	for i, x := range res.Series {
		pts := make([]gen.MetricPoint, len(x.Points))
		for j, pt := range x.Points {
			pts[j] = gen.MetricPoint{T: pt.T, V: pt.V}
		}
		out.Series[i] = gen.MetricQuerySeries{DeviceId: x.DeviceID, CheckId: x.CheckID, Object: x.Object,
			Name: x.Name, Unit: x.Unit, SeriesIds: x.SeriesIDs, Points: pts}
	}
	return out, nil
}

func toAPIRetention(c metrics.Class) gen.RetentionClass {
	return gen.RetentionClass{Name: c.Name, RawDays: c.Keep.Raw, Rollup5mDays: c.Keep.FiveMinute, Rollup1hDays: c.Keep.Hourly}
}

// ListRetentionClasses is open to every caller: class names and retention
// are not tenant data.
func (s *Server) ListRetentionClasses(ctx context.Context, _ gen.ListRetentionClassesRequestObject) (gen.ListRetentionClassesResponseObject, error) {
	var list []metrics.Class
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		list, err = metrics.ListClasses(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := gen.ListRetentionClasses200JSONResponse{Items: make([]gen.RetentionClass, len(list))}
	for i, c := range list {
		out.Items[i] = toAPIRetention(c)
	}
	return out, nil
}

func (s *Server) PutRetentionClass(ctx context.Context, req gen.PutRetentionClassRequestObject) (gen.PutRetentionClassResponseObject, error) {
	p, err := platformScope(ctx)
	if err != nil {
		return nil, err
	}
	var platform uuid.UUID
	if err := s.db.QueryRow(ctx, `SELECT platform_tenant_id()`).Scan(&platform); err != nil {
		return nil, err
	}
	b := req.Body
	keep := metrics.Keep{Raw: b.RawDays, FiveMinute: b.Rollup5mDays, Hourly: b.Rollup1hDays}
	var c metrics.Class
	var created bool
	err = store.InTenant(ctx, s.db, platform, func(tx pgx.Tx) error {
		c, created, err = metrics.PutClass(ctx, tx, p.Actor, platform, req.Name, keep)
		return err
	})
	if err != nil {
		return nil, err
	}
	if created {
		return gen.PutRetentionClass201JSONResponse(toAPIRetention(c)), nil
	}
	return gen.PutRetentionClass200JSONResponse(toAPIRetention(c)), nil
}
