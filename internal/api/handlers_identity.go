package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/auth"
	"github.com/plusclouds/monitoring.server/internal/store"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

func (s *Server) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	p, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	me := gen.Me{Tenant: toAPITenant(*t), Role: gen.MeRole(p.Role), ApiKeyId: &p.KeyID}
	switch {
	case p.User != nil:
		u := toAPIUser(*p.User)
		me.User, me.ActorKind = &u, gen.MeActorKindUser
	case p.Platform:
		me.ActorKind = gen.MeActorKindPlatform
	default:
		me.ActorKind = gen.MeActorKindApiKey
	}
	return gen.GetMe200JSONResponse(me), nil
}

func (s *Server) GetTenant(ctx context.Context, _ gen.GetTenantRequestObject) (gen.GetTenantResponseObject, error) {
	_, t, err := tenantScope(ctx, roleReadOnly, false)
	if err != nil {
		return nil, err
	}
	return gen.GetTenant200JSONResponse(toAPITenant(*t)), nil
}

const keyCols = `id, name, role, prefix, expires_at, ip_allowlist, last_used_at, created_at`

func scanKey(r pgx.Row) (gen.APIKey, error) {
	var k gen.APIKey
	var nets []netip.Prefix
	var role string
	err := r.Scan(&k.Id, &k.Name, &role, &k.Prefix, &k.ExpiresAt, &nets, &k.LastUsedAt, &k.CreatedAt)
	k.Role = gen.Role(role)
	k.IpAllowlist = make([]string, len(nets))
	for i, n := range nets {
		k.IpAllowlist[i] = n.String()
	}
	return k, err
}

func (s *Server) ListAPIKeys(ctx context.Context, req gen.ListAPIKeysRequestObject) (gen.ListAPIKeysResponseObject, error) {
	_, t, err := tenantScope(ctx, roleAdmin, false)
	if err != nil {
		return nil, err
	}
	var staleDays *int
	if req.Params.Stale != nil {
		staleDays = req.Params.Stale
	}
	var items []gen.APIKey
	err = store.InTenant(ctx, s.db, t.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+keyCols+` FROM api_keys
			 WHERE tenant_id = $1 AND revoked_at IS NULL
			   AND ($2::int IS NULL OR expires_at IS NULL OR last_used_at IS NULL
			        OR last_used_at < now() - make_interval(days => $2::int))
			 ORDER BY created_at`, t.ID, staleDays)
		if err != nil {
			return err
		}
		items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (gen.APIKey, error) { return scanKey(r) })
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.ListAPIKeys200JSONResponse{Items: nonNilSlice(items)}, nil
}

func (s *Server) CreateAPIKey(ctx context.Context, req gen.CreateAPIKeyRequestObject) (gen.CreateAPIKeyResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	b := req.Body
	var nets []netip.Prefix
	if b.IpAllowlist != nil {
		if nets, err = tenancy.ParseNetworks(*b.IpAllowlist); err != nil {
			return nil, err
		}
	}
	if b.ExpiresAt != nil && !b.ExpiresAt.After(time.Now()) {
		return nil, problem(http.StatusUnprocessableEntity, "invalid-value", "Invalid value", "expires_at must be in the future.")
	}
	if maxDays := t.Limits.MaxAPIKeyLifetimeDays; maxDays != nil {
		limit := time.Now().Add(time.Duration(*maxDays) * 24 * time.Hour)
		if b.ExpiresAt == nil || b.ExpiresAt.After(limit) {
			return nil, problem(http.StatusUnprocessableEntity, "key-lifetime", "Key lifetime too long",
				fmt.Sprintf("This tenant requires expires_at within %d days.", *maxDays))
		}
	}
	nk, err := auth.GenerateKey()
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	var k gen.APIKey
	err = store.InTenant(ctx, s.db, t.ID, func(tx pgx.Tx) error {
		if k, err = scanKey(tx.QueryRow(ctx, `
			INSERT INTO api_keys (id, tenant_id, kind, name, prefix, hash, role, expires_at, ip_allowlist)
			VALUES ($1, $2, 'tenant', $3, $4, $5, $6, $7, $8)
			RETURNING `+keyCols, id, t.ID, b.Name, nk.Prefix, nk.Hash, string(b.Role), b.ExpiresAt, nonNilSlice(nets))); err != nil {
			return err
		}
		return audit.Write(ctx, tx, p.Actor.Event(t.ID, "api_key.create", "api_key", id.String(), nil, map[string]any{
			"name": k.Name, "role": k.Role, "prefix": k.Prefix, "expires_at": k.ExpiresAt, "ip_allowlist": k.IpAllowlist,
		}))
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateAPIKey201JSONResponse{
		Id: k.Id, Name: k.Name, Role: k.Role, Prefix: k.Prefix, ExpiresAt: k.ExpiresAt,
		IpAllowlist: k.IpAllowlist, LastUsedAt: k.LastUsedAt, CreatedAt: k.CreatedAt, Key: nk.Full,
	}, nil
}

func (s *Server) RevokeAPIKey(ctx context.Context, req gen.RevokeAPIKeyRequestObject) (gen.RevokeAPIKeyResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, true)
	if err != nil {
		return nil, err
	}
	err = store.InTenant(ctx, s.db, t.ID, func(tx pgx.Tx) error {
		var name string
		err := tx.QueryRow(ctx, `UPDATE api_keys SET revoked_at = now()
			WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL RETURNING name`, req.KeyId, t.ID).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, p.Actor.Event(t.ID, "api_key.revoke", "api_key", req.KeyId.String(),
			map[string]any{"name": name}, nil))
	})
	if err != nil {
		return nil, err
	}
	return gen.RevokeAPIKey204Response{}, nil
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
