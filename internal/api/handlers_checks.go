package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/credential"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

func checkLimits(t *tenancy.Tenant) inventory.Limits {
	return inventory.Limits{MaxChecks: t.Limits.MaxChecks, MinCheckIntervalSeconds: t.Limits.MinCheckIntervalSeconds}
}

func (s *Server) listChecks(ctx context.Context, f inventory.CheckFilter, cursor *string, limit *int) (gen.CheckPage, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return gen.CheckPage{}, err
	}
	p, n, err := page(cursor, limit)
	if err != nil {
		return gen.CheckPage{}, err
	}
	var items []inventory.Check
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		if f.DeviceID != nil {
			if _, err := inventory.GetDevice(ctx, tx, *f.DeviceID); err != nil {
				return err
			}
		}
		items, err = inventory.ListChecks(ctx, tx, f, p)
		return err
	})
	if err != nil {
		return gen.CheckPage{}, err
	}
	items, next := trim(items, n, func(x inventory.Check) uuid.UUID { return x.ID })
	out := gen.CheckPage{Items: make([]gen.Check, len(items)), NextCursor: next}
	for i, x := range items {
		out.Items[i] = toAPICheck(x)
	}
	return out, nil
}

func (s *Server) ListChecks(ctx context.Context, req gen.ListChecksRequestObject) (gen.ListChecksResponseObject, error) {
	q := req.Params
	out, err := s.listChecks(ctx, inventory.CheckFilter{DeviceID: q.DeviceId, Plugin: q.Plugin, Enabled: q.Enabled}, q.Cursor, q.Limit)
	if err != nil {
		return nil, err
	}
	return gen.ListChecks200JSONResponse(out), nil
}

func (s *Server) ListDeviceChecks(ctx context.Context, req gen.ListDeviceChecksRequestObject) (gen.ListDeviceChecksResponseObject, error) {
	out, err := s.listChecks(ctx, inventory.CheckFilter{DeviceID: &req.DeviceId}, req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	return gen.ListDeviceChecks200JSONResponse(out), nil
}

func (s *Server) CreateCheck(ctx context.Context, req gen.CreateCheckRequestObject) (gen.CreateCheckResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	in, err := checkInput(*req.Body)
	if err != nil {
		return nil, err
	}
	var c inventory.Check
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = inventory.CreateCheck(ctx, tx, p.Actor, req.DeviceId, checkLimits(t), in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateCheck201JSONResponse(toAPICheck(c)), nil
}

func (s *Server) GetCheck(ctx context.Context, req gen.GetCheckRequestObject) (gen.GetCheckResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var c inventory.Check
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = inventory.GetCheck(ctx, tx, req.CheckId)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.GetCheck200JSONResponse(toAPICheck(c)), nil
}

func (s *Server) UpdateCheck(ctx context.Context, req gen.UpdateCheckRequestObject) (gen.UpdateCheckResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	in, err := checkInput(*req.Body)
	if err != nil {
		return nil, err
	}
	var c inventory.Check
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = inventory.UpdateCheck(ctx, tx, p.Actor, req.CheckId, checkLimits(t), in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.UpdateCheck200JSONResponse(toAPICheck(c)), nil
}

func (s *Server) DeleteCheck(ctx context.Context, req gen.DeleteCheckRequestObject) (gen.DeleteCheckResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, t, func(tx pgx.Tx) error { return inventory.DeleteCheck(ctx, tx, p.Actor, req.CheckId) })
	if err != nil {
		return nil, err
	}
	return gen.DeleteCheck204Response{}, nil
}

// ---- credentials ----

func (s *Server) ListCredentialTypes(ctx context.Context, _ gen.ListCredentialTypesRequestObject) (gen.ListCredentialTypesResponseObject, error) {
	if _, _, err := tenantScope(ctx, roleReadOnly, false); err != nil {
		return nil, err
	}
	all := credential.All()
	out := gen.ListCredentialTypes200JSONResponse{Items: make([]gen.CredentialType, len(all))}
	for i, t := range all {
		var schema map[string]any
		if err := json.Unmarshal(t.Schema(), &schema); err != nil {
			return nil, err
		}
		out.Items[i] = gen.CredentialType{Name: t.Name, Description: t.Description, FieldsSchema: schema}
	}
	return out, nil
}

func (s *Server) ListCredentials(ctx context.Context, req gen.ListCredentialsRequestObject) (gen.ListCredentialsResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	q := req.Params
	p, n, err := page(q.Cursor, q.Limit)
	if err != nil {
		return nil, err
	}
	var items []credential.Credential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		items, err = credential.List(ctx, tx, credential.Filter{
			Type: q.Type, External: filterKey(q.ExternalSource, q.ExternalType, q.ExternalId),
			After: p.After, Limit: p.Limit,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	items, next := trim(items, n, func(x credential.Credential) uuid.UUID { return x.ID })
	out := gen.ListCredentials200JSONResponse{Items: make([]gen.Credential, len(items)), NextCursor: next}
	for i, x := range items {
		out.Items[i] = toAPICredential(x)
	}
	return out, nil
}

func (s *Server) CreateCredential(ctx context.Context, req gen.CreateCredentialRequestObject) (gen.CreateCredentialResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	var c credential.Credential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = s.creds.Create(ctx, tx, p.Actor, t.ID, credentialInput(*req.Body))
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateCredential201JSONResponse(toAPICredential(c)), nil
}

func (s *Server) UpsertCredential(ctx context.Context, req gen.UpsertCredentialRequestObject) (gen.UpsertCredentialResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	k := s.key(req.Params.Source, req.Params.Type, req.ExternalId)
	in := credentialInput(*req.Body)
	in.External = k.Ref()
	var c credential.Credential
	var created bool
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		cur, err := credential.ByExternal(ctx, tx, k)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			created = true
			c, err = s.creds.Create(ctx, tx, p.Actor, t.ID, in)
		case err == nil:
			in.External = keepType(cur.External, k)
			c, err = s.creds.Update(ctx, tx, p.Actor, cur.ID, in)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if created {
		return gen.UpsertCredential201JSONResponse(toAPICredential(c)), nil
	}
	return gen.UpsertCredential200JSONResponse(toAPICredential(c)), nil
}

func (s *Server) GetCredential(ctx context.Context, req gen.GetCredentialRequestObject) (gen.GetCredentialResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	var c credential.Credential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = credential.Get(ctx, tx, req.CredentialId)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.GetCredential200JSONResponse(toAPICredential(c)), nil
}

func (s *Server) UpdateCredential(ctx context.Context, req gen.UpdateCredentialRequestObject) (gen.UpdateCredentialResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	var c credential.Credential
	err = s.tx(ctx, t, func(tx pgx.Tx) error {
		c, err = s.creds.Update(ctx, tx, p.Actor, req.CredentialId, credentialInput(*req.Body))
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.UpdateCredential200JSONResponse(toAPICredential(c)), nil
}

func (s *Server) DeleteCredential(ctx context.Context, req gen.DeleteCredentialRequestObject) (gen.DeleteCredentialResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, t, func(tx pgx.Tx) error { return credential.Delete(ctx, tx, p.Actor, req.CredentialId) })
	if err != nil {
		return nil, err
	}
	return gen.DeleteCredential204Response{}, nil
}
