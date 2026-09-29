package api

import (
	"context"
	"net/netip"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

// Roles in increasing order of rights. "platform" is the platform key
// acting without a named user: full rights in the tenant.
const (
	roleReadOnly = "read-only"
	roleOperator = "operator"
	roleAdmin    = "admin"
	rolePlatform = "platform"
)

var roleRank = map[string]int{roleReadOnly: 1, roleOperator: 2, roleAdmin: 3, rolePlatform: 4}

// Principal is who is calling and on behalf of which tenant.
type Principal struct {
	KeyID    uuid.UUID
	Platform bool            // authenticated with the platform key
	Tenant   *tenancy.Tenant // nil for a platform key without a tenant header
	User     *tenancy.User   // the acting user of a platform-key request
	Role     string
	Actor    audit.Actor
}

// allows reports whether the principal's role is at least min.
func (p *Principal) allows(min string) bool { return roleRank[p.Role] >= roleRank[min] }

type ctxKey int

const (
	principalKey ctxKey = iota
	requestIDKey
	clientIPKey
)

func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func principal(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey).(*Principal)
	return p
}

func requestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

func clientIP(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(clientIPKey).(netip.Addr)
	return a
}

// tenantScope returns the principal and its tenant, requiring min role.
// Writes to a suspended tenant are refused (F01).
func tenantScope(ctx context.Context, min string, write bool) (*Principal, *tenancy.Tenant, error) {
	p := principal(ctx)
	if p.Tenant == nil {
		return nil, nil, errTenantRequired
	}
	if !p.allows(min) {
		return nil, nil, errForbidden
	}
	if write && p.Tenant.Status == tenancy.StatusSuspended {
		return nil, nil, errTenantSuspended
	}
	return p, p.Tenant, nil
}

// platformScope requires the platform key.
func platformScope(ctx context.Context) (*Principal, error) {
	p := principal(ctx)
	if !p.Platform {
		return nil, errPlatformOnly
	}
	return p, nil
}
