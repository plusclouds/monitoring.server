package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/execute"
	"github.com/plusclouds/monitoring.server/internal/extref"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/store"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// page turns cursor and limit parameters into a page that fetches one extra
// row, so the handler knows whether there is a next page.
func page(cursor *string, limit *int) (inventory.Page, int, error) {
	n := 100
	if limit != nil {
		n = *limit
	}
	p := inventory.Page{Limit: n + 1}
	if cursor != nil {
		id, err := decodeCursorID(*cursor)
		if err != nil {
			return p, n, err
		}
		p.After = &id
	}
	return p, n, nil
}

// trim cuts a fetched page to n items and returns the next cursor.
func trim[T any](items []T, n int, id func(T) uuid.UUID) ([]T, *string) {
	if len(items) <= n {
		return nonNilSlice(items), nil
	}
	c := encodeCursorID(id(items[n-1]))
	return items[:n], &c
}

// filterKey builds an external ID filter from list parameters.
func filterKey(source, typ, id *string) *extref.Key {
	if source == nil && typ == nil && id == nil {
		return nil
	}
	k := &extref.Key{Type: typ}
	if source != nil {
		k.Source = *source
	}
	if id != nil {
		k.ID = *id
	}
	return k
}

// key is the external ID named by a by-external-id path; the source defaults
// to the platform's identity source.
func (s *Server) key(source *string, typ *string, id string) extref.Key {
	return extref.Key{Source: s.source(source), Type: typ, ID: id}
}

// tx runs fn in a transaction scoped to the tenant.
func (s *Server) tx(ctx context.Context, t *tenancy.Tenant, fn func(pgx.Tx) error) error {
	return store.InTenant(ctx, s.db, t.ID, fn)
}

// ---- sites ----

func (s *Server) ListSites(ctx context.Context, req gen.ListSitesRequestObject) (gen.ListSitesResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	q := req.Params
	p, n, err := page(q.Cursor, q.Limit)
	if err != nil {
		return nil, err
	}
	var items []inventory.Site
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		items, err = inventory.ListSites(ctx, tx, p, filterKey(q.ExternalSource, q.ExternalType, q.ExternalId))
		return err
	})
	if err != nil {
		return nil, err
	}
	items, next := trim(items, n, func(x inventory.Site) uuid.UUID { return x.ID })
	out := gen.ListSites200JSONResponse{Items: make([]gen.Site, len(items)), NextCursor: next}
	for i, x := range items {
		out.Items[i] = toAPISite(x)
	}
	return out, nil
}

func (s *Server) CreateSite(ctx context.Context, req gen.CreateSiteRequestObject) (gen.CreateSiteResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	var site inventory.Site
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		site, err = inventory.CreateSite(ctx, tx, p.Actor, t.ID, siteInput(*req.Body))
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateSite201JSONResponse(toAPISite(site)), nil
}

func (s *Server) UpsertSite(ctx context.Context, req gen.UpsertSiteRequestObject) (gen.UpsertSiteResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	k := s.key(req.Params.Source, req.Params.Type, req.ExternalId)
	in := siteInput(*req.Body)
	in.External = k.Ref()
	var site inventory.Site
	var created bool
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err := inventory.SiteByExternal(ctx, tx, k)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			created = true
			site, err = inventory.CreateSite(ctx, tx, p.Actor, t.ID, in)
		case err == nil:
			in.External = keepType(cur.External, k)
			site, err = inventory.UpdateSite(ctx, tx, p.Actor, cur.ID, in)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if created {
		return gen.UpsertSite201JSONResponse(toAPISite(site)), nil
	}
	return gen.UpsertSite200JSONResponse(toAPISite(site)), nil
}

// keepType keeps the stored external type when the upsert did not name one.
func keepType(stored *extref.Ref, k extref.Key) *extref.Ref {
	r := k.Ref()
	if r.Type == nil && stored != nil {
		r.Type = stored.Type
	}
	return r
}

func (s *Server) GetSite(ctx context.Context, req gen.GetSiteRequestObject) (gen.GetSiteResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var site inventory.Site
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		site, err = inventory.GetSite(ctx, tx, req.SiteId)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.GetSite200JSONResponse(toAPISite(site)), nil
}

func (s *Server) UpdateSite(ctx context.Context, req gen.UpdateSiteRequestObject) (gen.UpdateSiteResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	var site inventory.Site
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		site, err = inventory.UpdateSite(ctx, tx, p.Actor, req.SiteId, siteInput(*req.Body))
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.UpdateSite200JSONResponse(toAPISite(site)), nil
}

func (s *Server) DeleteSite(ctx context.Context, req gen.DeleteSiteRequestObject) (gen.DeleteSiteResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, t, func(tx pgx.Tx) error { return inventory.DeleteSite(ctx, tx, p.Actor, req.SiteId) })
	if err != nil {
		return nil, err
	}
	return gen.DeleteSite204Response{}, nil
}

// ---- devices ----

func availability(in *[]gen.Availability) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(*in))
	for i, a := range *in {
		out[i] = string(a)
	}
	return out
}

func parseTags(in *[]string) (map[string]string, error) {
	if in == nil {
		return nil, nil
	}
	out := make(map[string]string, len(*in))
	for _, kv := range *in {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, problem(http.StatusBadRequest, "invalid-request", "Invalid request", "tag must be key=value")
		}
		out[k] = v
	}
	return out, nil
}

func (s *Server) listDevices(ctx context.Context, f inventory.DeviceFilter, cursor *string, limit *int) (gen.DevicePage, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return gen.DevicePage{}, err
	}
	p, n, err := page(cursor, limit)
	if err != nil {
		return gen.DevicePage{}, err
	}
	var items []inventory.Device
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if f.ParentID != nil {
			if _, err := inventory.GetDevice(ctx, tx, *f.ParentID); err != nil {
				return err
			}
		}
		items, err = inventory.ListDevices(ctx, tx, f, p)
		return err
	})
	if err != nil {
		return gen.DevicePage{}, err
	}
	items, next := trim(items, n, func(x inventory.Device) uuid.UUID { return x.ID })
	out := gen.DevicePage{Items: make([]gen.Device, len(items)), NextCursor: next}
	for i, x := range items {
		out.Items[i] = toAPIDevice(x)
	}
	return out, nil
}

func (s *Server) ListDevices(ctx context.Context, req gen.ListDevicesRequestObject) (gen.ListDevicesResponseObject, error) {
	q := req.Params
	tags, err := parseTags(q.Tag)
	if err != nil {
		return nil, err
	}
	out, err := s.listDevices(ctx, inventory.DeviceFilter{
		Type: q.Type, SiteID: q.SiteId, ParentID: q.ParentId, Tags: tags, Availability: availability(q.Availability),
		External: filterKey(q.ExternalSource, q.ExternalType, q.ExternalId),
	}, q.Cursor, q.Limit)
	if err != nil {
		return nil, err
	}
	return gen.ListDevices200JSONResponse(out), nil
}

func (s *Server) ListDeviceChildren(ctx context.Context, req gen.ListDeviceChildrenRequestObject) (gen.ListDeviceChildrenResponseObject, error) {
	out, err := s.listDevices(ctx, inventory.DeviceFilter{ParentID: &req.DeviceId}, req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	return gen.ListDeviceChildren200JSONResponse(out), nil
}

func (s *Server) CreateDevice(ctx context.Context, req gen.CreateDeviceRequestObject) (gen.CreateDeviceResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	var d inventory.Device
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if d, err = inventory.CreateDevice(ctx, tx, p.Actor, t.ID, t.Limits.MaxDevices, deviceInput(*req.Body)); err != nil {
			return err
		}
		d, err = withStatus(ctx, tx, d)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateDevice201JSONResponse(toAPIDevice(d)), nil
}

func (s *Server) UpsertDevice(ctx context.Context, req gen.UpsertDeviceRequestObject) (gen.UpsertDeviceResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	k := s.key(req.Params.Source, req.Params.Type, req.ExternalId)
	in := deviceInput(*req.Body)
	in.External = k.Ref()
	var d inventory.Device
	var created bool
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err := inventory.DeviceByExternal(ctx, tx, k)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			created = true
			d, err = inventory.CreateDevice(ctx, tx, p.Actor, t.ID, t.Limits.MaxDevices, in)
		case err == nil:
			in.External = keepType(cur.External, k)
			d, err = inventory.UpdateDevice(ctx, tx, p.Actor, cur.ID, in)
		}
		if err != nil {
			return err
		}
		d, err = withStatus(ctx, tx, d)
		return err
	})
	if err != nil {
		return nil, err
	}
	if created {
		return gen.UpsertDevice201JSONResponse(toAPIDevice(d)), nil
	}
	return gen.UpsertDevice200JSONResponse(toAPIDevice(d)), nil
}

func (s *Server) GetDevice(ctx context.Context, req gen.GetDeviceRequestObject) (gen.GetDeviceResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var d inventory.Device
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if d, err = inventory.GetDevice(ctx, tx, req.DeviceId); err != nil {
			return err
		}
		d, err = withStatus(ctx, tx, d)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.GetDevice200JSONResponse(toAPIDevice(d)), nil
}

func (s *Server) UpdateDevice(ctx context.Context, req gen.UpdateDeviceRequestObject) (gen.UpdateDeviceResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	var d inventory.Device
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if d, err = inventory.UpdateDevice(ctx, tx, p.Actor, req.DeviceId, deviceInput(*req.Body)); err != nil {
			return err
		}
		d, err = withStatus(ctx, tx, d)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.UpdateDevice200JSONResponse(toAPIDevice(d)), nil
}

func (s *Server) DeleteDevice(ctx context.Context, req gen.DeleteDeviceRequestObject) (gen.DeleteDeviceResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	confirm := req.Params.Confirm != nil && *req.Params.Confirm
	err = s.tx(ctx, t, func(tx pgx.Tx) error { return inventory.DeleteDevice(ctx, tx, p.Actor, req.DeviceId, confirm) })
	if err != nil {
		return nil, err
	}
	return gen.DeleteDevice204Response{}, nil
}

// withStatus adds the rolled-up status to one device.
func withStatus(ctx context.Context, tx pgx.Tx, d inventory.Device) (inventory.Device, error) {
	devs := []inventory.Device{d}
	err := inventory.FillStatus(ctx, tx, devs)
	return devs[0], err
}

// ---- dependencies ----

func (s *Server) dependencyList(ctx context.Context, device uuid.UUID,
	fn func(context.Context, pgx.Tx, uuid.UUID) ([]inventory.Dependency, error)) (gen.DependencyList, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return gen.DependencyList{}, err
	}
	var deps []inventory.Dependency
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		deps, err = fn(ctx, tx, device)
		return err
	})
	if err != nil {
		return gen.DependencyList{}, err
	}
	out := gen.DependencyList{Items: make([]gen.Dependency, len(deps))}
	for i, d := range deps {
		out.Items[i] = toAPIDependency(d)
	}
	return out, nil
}

func (s *Server) ListDeviceDependencies(ctx context.Context, req gen.ListDeviceDependenciesRequestObject) (gen.ListDeviceDependenciesResponseObject, error) {
	out, err := s.dependencyList(ctx, req.DeviceId, inventory.Dependencies)
	if err != nil {
		return nil, err
	}
	return gen.ListDeviceDependencies200JSONResponse(out), nil
}

func (s *Server) ListDeviceDependents(ctx context.Context, req gen.ListDeviceDependentsRequestObject) (gen.ListDeviceDependentsResponseObject, error) {
	out, err := s.dependencyList(ctx, req.DeviceId, inventory.Dependents)
	if err != nil {
		return nil, err
	}
	return gen.ListDeviceDependents200JSONResponse(out), nil
}

func (s *Server) AddDeviceDependency(ctx context.Context, req gen.AddDeviceDependencyRequestObject) (gen.AddDeviceDependencyResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	var added bool
	var dep inventory.Dependency
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if added, err = inventory.AddDependency(ctx, tx, p.Actor, req.DeviceId, req.Body.DependsOnId); err != nil {
			return err
		}
		deps, err := inventory.Dependencies(ctx, tx, req.DeviceId)
		for _, d := range deps {
			if d.DependsOnID == req.Body.DependsOnId && d.Source != "containment" {
				dep = d
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if added {
		return gen.AddDeviceDependency201JSONResponse(toAPIDependency(dep)), nil
	}
	return gen.AddDeviceDependency200JSONResponse(toAPIDependency(dep)), nil
}

func (s *Server) RemoveDeviceDependency(ctx context.Context, req gen.RemoveDeviceDependencyRequestObject) (gen.RemoveDeviceDependencyResponseObject, error) {
	p, t, err := tenantScope(ctx, roleConfig, true)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		return inventory.RemoveDependency(ctx, tx, p.Actor, req.DeviceId, req.DependsOnId)
	})
	if err != nil {
		return nil, err
	}
	return gen.RemoveDeviceDependency204Response{}, nil
}

func (s *Server) GetDeviceImpact(ctx context.Context, req gen.GetDeviceImpactRequestObject) (gen.GetDeviceImpactResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var items []inventory.Impacted
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if items, err = inventory.Impact(ctx, tx, req.DeviceId); err != nil {
			return err
		}
		devs := make([]inventory.Device, len(items))
		for i := range items {
			devs[i] = items[i].Device
		}
		if err := inventory.FillStatus(ctx, tx, devs); err != nil {
			return err
		}
		for i := range items {
			items[i].Device = devs[i]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := gen.GetDeviceImpact200JSONResponse{Items: make([]struct {
		Device   gen.Device `json:"device"`
		Distance int        `json:"distance"`
	}, len(items))}
	for i, x := range items {
		out.Items[i].Device, out.Items[i].Distance = toAPIDevice(x.Device), x.Distance
	}
	return out, nil
}

// ---- device test ----

// deviceTestTimeout bounds POST /devices/{id}/test (F02).
const deviceTestTimeout = 60 * time.Second

func (s *Server) TestDevice(ctx context.Context, req gen.TestDeviceRequestObject) (gen.TestDeviceResponseObject, error) {
	_, t, err := tenantScope(ctx, roleOperator, false)
	if err != nil {
		return nil, err
	}
	type prepared struct {
		check inventory.Check
		job   execute.Job
		err   error
	}
	var jobs []prepared
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		d, err := inventory.GetDevice(ctx, tx, req.DeviceId)
		if err != nil {
			return err
		}
		enabled := true
		checks, err := inventory.ListChecks(ctx, tx, inventory.CheckFilter{DeviceID: &d.ID, Enabled: &enabled},
			inventory.Page{Limit: 1000})
		if err != nil {
			return err
		}
		for _, c := range checks {
			j := execute.Job{
				Tenant:   execute.Tenant{ID: t.ID, IsPlatform: t.IsPlatform, AllowedTargetNetworks: t.Limits.AllowedTargetNetworks},
				DeviceID: d.ID, Address: d.Address, CheckID: c.ID, Plugin: c.Plugin, Config: c.Config,
				IntervalSeconds: c.IntervalSeconds, TimeoutSeconds: c.TimeoutSeconds, Credentials: c.Credentials,
			}
			// Decrypt now, inside the tenant's transaction; the runs happen after it.
			var derr error
			j.Decrypted, derr = s.exec.Decrypt(ctx, tx, j)
			jobs = append(jobs, prepared{check: c, job: j, err: derr})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, deviceTestTimeout)
	defer cancel()
	out := gen.TestDevice200JSONResponse{Items: make([]gen.CheckRun, len(jobs))}
	var wg sync.WaitGroup
	for i, p := range jobs {
		if p.err != nil {
			out.Items[i] = toAPICheckRun(p.check, plugin.Result{Status: plugin.Unknown, Output: p.err.Error(), Time: time.Now()})
			continue
		}
		wg.Go(func() { out.Items[i] = toAPICheckRun(p.check, s.exec.Run(runCtx, nil, p.job)) })
	}
	wg.Wait()
	return out, nil
}
