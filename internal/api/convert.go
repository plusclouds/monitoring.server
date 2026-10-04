package api

import (
	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

func toAPITenant(t tenancy.Tenant) gen.Tenant {
	nets := make([]string, len(t.Limits.AllowedTargetNetworks))
	for i, n := range t.Limits.AllowedTargetNetworks {
		nets[i] = n.String()
	}
	classes := append([]string{}, t.Limits.MetricClasses...)
	l := t.Limits
	return gen.Tenant{
		Id:           t.ID,
		Name:         t.Name,
		Status:       gen.TenantStatus(t.Status),
		IsPlatform:   t.IsPlatform,
		ConfigSource: gen.TenantConfigSource(t.ConfigSource),
		Limits: gen.TenantLimits{
			MaxDevices: &l.MaxDevices, MaxChecks: &l.MaxChecks,
			MaxWebhooks: &l.MaxWebhooks, MaxAlertRoutes: &l.MaxAlertRoutes,
			MinCheckIntervalSeconds: &l.MinCheckIntervalSeconds, ApiRatePerMinute: &l.APIRatePerMinute,
			AllowedTargetNetworks: &nets, MetricClasses: &classes,
			MaxApiKeyLifetimeDays: l.MaxAPIKeyLifetimeDays,
		},
		External:    external(t.ExternalSource, t.ExternalType, t.ExternalID),
		Provisioned: gen.TenantProvisioned(t.Provisioned),
		CreatedAt:   t.CreatedAt.UTC(),
		UpdatedAt:   t.UpdatedAt.UTC(),
	}
}

func toAPIUser(u tenancy.User) gen.User {
	return gen.User{
		Id:          u.ID,
		DisplayName: u.DisplayName,
		Status:      gen.UserStatus(u.Status),
		External:    external(u.ExternalSource, u.ExternalType, u.ExternalID),
	}
}

func toAPIMember(m tenancy.Member) gen.Member {
	return gen.Member{User: toAPIUser(m.User), Role: gen.Role(m.Role), CreatedAt: m.CreatedAt.UTC()}
}

func external(source, typ, id *string) *gen.ExternalRef {
	if source == nil || id == nil {
		return nil
	}
	return &gen.ExternalRef{Source: *source, Type: typ, Id: *id}
}

func limitsPatch(l *gen.TenantLimits) tenancy.LimitsPatch {
	if l == nil {
		return tenancy.LimitsPatch{}
	}
	return tenancy.LimitsPatch{
		MaxDevices: l.MaxDevices, MaxChecks: l.MaxChecks, MaxWebhooks: l.MaxWebhooks, MaxAlertRoutes: l.MaxAlertRoutes,
		MinCheckIntervalSeconds: l.MinCheckIntervalSeconds, APIRatePerMinute: l.ApiRatePerMinute,
		AllowedTargetNetworks: l.AllowedTargetNetworks, MetricClasses: l.MetricClasses,
		MaxAPIKeyLifetimeDays: l.MaxApiKeyLifetimeDays,
	}
}
