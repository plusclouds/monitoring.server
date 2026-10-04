package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"

	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

// mergePatch applies a JSON Merge Patch (RFC 7396) to target: null removes
// a member, objects merge recursively, anything else replaces.
func mergePatch(target, patch map[string]any) map[string]any {
	if target == nil {
		target = map[string]any{}
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(target, k)
		case map[string]any:
			cur, _ := target[k].(map[string]any)
			target[k] = mergePatch(cur, pv)
		default:
			target[k] = v
		}
	}
	return target
}

// patched applies a merge patch to the write form of an object (doc) and
// decodes the result back into it, rejecting fields the write form does not
// have.
func patched[T any](doc T, patch map[string]any) (T, error) {
	var out T
	cur, err := via[map[string]any](doc)
	if err != nil {
		return out, err
	}
	b, err := json.Marshal(mergePatch(cur, patch))
	if err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, errs.Invalidf("patch", "%s", err.Error())
	}
	return out, nil
}

func patchBody(a, b *gen.MergePatch) map[string]any {
	switch {
	case b != nil:
		return *b
	case a != nil:
		return *a
	}
	return map[string]any{}
}

// deviceWrite is a device's client-writable fields in their write form.
func deviceWrite(d inventory.Device) gen.DeviceWrite {
	w := gen.DeviceWrite{Name: d.Name, Type: gen.DeviceType(d.Type), Notes: d.Notes, ParentId: d.ParentID,
		SiteId: d.SiteID, PhysicalPeerId: d.PhysicalPeerID}
	if d.Address != "" {
		w.Address = &d.Address
	}
	if len(d.Tags) > 0 {
		w.Tags = &d.Tags
	}
	if r := d.External; r != nil {
		w.External = &gen.ExternalRefWrite{Source: r.Source, Type: r.Type, Id: r.ID}
	}
	return w
}

// patchDevice locks the device, applies the patch and saves it like a PUT.
func (s *Server) patchDevice(ctx context.Context, t *tenancy.Tenant, p *Principal,
	find func(pgx.Tx) (inventory.Device, error), patch map[string]any) (gen.Device, error) {
	var d inventory.Device
	err := s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err := find(tx)
		if err != nil {
			return err
		}
		if cur, err = inventory.LockDevice(ctx, tx, cur.ID); err != nil {
			return err
		}
		w, err := patched(deviceWrite(cur), patch)
		if err != nil {
			return err
		}
		if d, err = inventory.UpdateDevice(ctx, tx, p.Actor, cur.ID, deviceInput(w)); err != nil {
			return err
		}
		d, err = withStatus(ctx, tx, d)
		return err
	})
	return toAPIDevice(d), err
}

func (s *Server) PatchDevice(ctx context.Context, req gen.PatchDeviceRequestObject) (gen.PatchDeviceResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	out, err := s.patchDevice(ctx, t, p, func(tx pgx.Tx) (inventory.Device, error) {
		return inventory.GetDevice(ctx, tx, req.DeviceId)
	}, patchBody(req.JSONBody, req.ApplicationMergePatchPlusJSONBody))
	if err != nil {
		return nil, err
	}
	return gen.PatchDevice200JSONResponse(out), nil
}

func (s *Server) PatchDeviceByExternalID(ctx context.Context, req gen.PatchDeviceByExternalIDRequestObject) (gen.PatchDeviceByExternalIDResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	k := s.key(req.Params.Source, req.Params.Type, req.ExternalId)
	out, err := s.patchDevice(ctx, t, p, func(tx pgx.Tx) (inventory.Device, error) {
		return inventory.DeviceByExternal(ctx, tx, k)
	}, patchBody(req.JSONBody, req.ApplicationMergePatchPlusJSONBody))
	if err != nil {
		return nil, err
	}
	return gen.PatchDeviceByExternalID200JSONResponse(out), nil
}

// checkWrite is a check's client-writable fields in their write form.
func checkWrite(c inventory.Check) (gen.CheckWrite, error) {
	w := gen.CheckWrite{Name: c.Name, Plugin: c.Plugin, IntervalSeconds: &c.IntervalSeconds,
		TimeoutSeconds: c.TimeoutSeconds, Enabled: &c.Enabled, FailureCount: &c.FailureCount,
		RecoveryCount: &c.RecoveryCount, IsHostCheck: &c.IsHostCheck, UnknownIsCritical: &c.UnknownIsCritical,
		RunbookUrl: c.RunbookURL}
	var cfg map[string]any
	if err := json.Unmarshal(c.Config, &cfg); err != nil {
		return w, err
	}
	w.Config = &cfg
	rules := rulesToAPI(c.Thresholds)
	w.Thresholds = &rules
	if len(c.Credentials) > 0 {
		creds := maps.Clone(c.Credentials)
		w.Credentials = &creds
	}
	return w, nil
}

func (s *Server) PatchCheck(ctx context.Context, req gen.PatchCheckRequestObject) (gen.PatchCheckResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	patch := patchBody(req.JSONBody, req.ApplicationMergePatchPlusJSONBody)
	var c inventory.Check
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err := inventory.LockCheck(ctx, tx, req.CheckId)
		if err != nil {
			return err
		}
		doc, err := checkWrite(cur)
		if err != nil {
			return err
		}
		w, err := patched(doc, patch)
		if err != nil {
			return err
		}
		in, err := checkInput(w)
		if err != nil {
			return err
		}
		c, err = inventory.UpdateCheck(ctx, tx, p.Actor, cur.ID, checkLimits(t), in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.PatchCheck200JSONResponse(toAPICheck(c)), nil
}
