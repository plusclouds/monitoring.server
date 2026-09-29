package api

import (
	"context"
	"encoding/base64"
	"net/http"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

// source returns the external source of a provisioning call, defaulting to
// the platform's identity source (ADR-0012).
func (s *Server) source(q *string) string {
	if q != nil && *q != "" {
		return *q
	}
	return s.identitySource
}

func (s *Server) ListTenants(ctx context.Context, req gen.ListTenantsRequestObject) (gen.ListTenantsResponseObject, error) {
	if _, err := platformScope(ctx); err != nil {
		return nil, err
	}
	limit := 100
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	var after *uuid.UUID
	if req.Params.Cursor != nil {
		id, err := decodeCursorID(*req.Params.Cursor)
		if err != nil {
			return nil, err
		}
		after = &id
	}
	ts, err := tenancy.List(ctx, s.db, req.Params.ExternalSource, req.Params.ExternalId, after, limit+1)
	if err != nil {
		return nil, err
	}
	resp := gen.ListTenants200JSONResponse{Items: []gen.Tenant{}}
	for i, t := range ts {
		if i == limit {
			c := encodeCursorID(ts[i-1].ID)
			resp.NextCursor = &c
			break
		}
		resp.Items = append(resp.Items, toAPITenant(t))
	}
	return resp, nil
}

func (s *Server) UpsertTenant(ctx context.Context, req gen.UpsertTenantRequestObject) (gen.UpsertTenantResponseObject, error) {
	p, err := platformScope(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	t, created, err := s.tenants.UpsertTenant(ctx, p.Actor, s.source(req.Params.Source), req.ExternalId,
		b.Name, b.ExternalType, limitsPatch(b.Limits))
	if err != nil {
		return nil, err
	}
	if created {
		return gen.UpsertTenant201JSONResponse(toAPITenant(t)), nil
	}
	return gen.UpsertTenant200JSONResponse(toAPITenant(t)), nil
}

func (s *Server) PatchTenant(ctx context.Context, req gen.PatchTenantRequestObject) (gen.PatchTenantResponseObject, error) {
	p, err := platformScope(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	var status *string
	if b.Status != nil {
		v := string(*b.Status)
		status = &v
	}
	t, err := s.tenants.PatchTenant(ctx, p.Actor, s.source(req.Params.Source), req.ExternalId, b.Name, status, limitsPatch(b.Limits))
	if err != nil {
		return nil, err
	}
	return gen.PatchTenant200JSONResponse(toAPITenant(t)), nil
}

func (s *Server) DeleteTenant(ctx context.Context, req gen.DeleteTenantRequestObject) (gen.DeleteTenantResponseObject, error) {
	p, err := platformScope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.tenants.DeleteTenant(ctx, p.Actor, s.source(req.Params.Source), req.ExternalId); err != nil {
		return nil, err
	}
	return gen.DeleteTenant204Response{}, nil
}

func (s *Server) ListMembers(ctx context.Context, req gen.ListMembersRequestObject) (gen.ListMembersResponseObject, error) {
	if _, err := platformScope(ctx); err != nil {
		return nil, err
	}
	ms, err := s.tenants.ListMembers(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	resp := gen.ListMembers200JSONResponse{Items: make([]gen.Member, len(ms))}
	for i, m := range ms {
		resp.Items[i] = toAPIMember(m)
	}
	return resp, nil
}

func (s *Server) UpsertMember(ctx context.Context, req gen.UpsertMemberRequestObject) (gen.UpsertMemberResponseObject, error) {
	p, err := platformScope(ctx)
	if err != nil {
		return nil, err
	}
	m, created, err := s.tenants.UpsertMember(ctx, p.Actor, req.TenantId, s.source(req.Params.Source),
		req.UserExternalId, string(req.Body.Role), req.Body.DisplayName)
	if err != nil {
		return nil, err
	}
	if created {
		return gen.UpsertMember201JSONResponse(toAPIMember(m)), nil
	}
	return gen.UpsertMember200JSONResponse(toAPIMember(m)), nil
}

func (s *Server) RemoveMember(ctx context.Context, req gen.RemoveMemberRequestObject) (gen.RemoveMemberResponseObject, error) {
	p, err := platformScope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.tenants.RemoveMember(ctx, p.Actor, req.TenantId, s.source(req.Params.Source), req.UserExternalId); err != nil {
		return nil, err
	}
	return gen.RemoveMember204Response{}, nil
}

func (s *Server) PatchUser(ctx context.Context, req gen.PatchUserRequestObject) (gen.PatchUserResponseObject, error) {
	p, err := platformScope(ctx)
	if err != nil {
		return nil, err
	}
	var status *string
	if req.Body.Status != nil {
		v := string(*req.Body.Status)
		status = &v
	}
	u, err := s.tenants.PatchUser(ctx, p.Actor, s.source(req.Params.Source), req.ExternalId,
		status, req.Body.DisplayName != nil, req.Body.DisplayName)
	if err != nil {
		return nil, err
	}
	return gen.PatchUser200JSONResponse(toAPIUser(u)), nil
}

// Cursors are opaque to clients (ADR-0007).
func encodeCursorID(id uuid.UUID) string { return base64.RawURLEncoding.EncodeToString(id[:]) }

func decodeCursorID(c string) (uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) != 16 {
		return uuid.Nil, problem(http.StatusBadRequest, "invalid-cursor", "Invalid cursor", "Use next_cursor from the previous page.")
	}
	return uuid.UUID(b), nil
}
