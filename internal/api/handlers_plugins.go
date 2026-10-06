package api

import (
	"context"
	"encoding/json"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// ListPlugins serves every registered plugin's manifest (F03). Any
// authenticated caller may read them.
func (s *Server) ListPlugins(_ context.Context, _ gen.ListPluginsRequestObject) (gen.ListPluginsResponseObject, error) {
	all := plugin.All()
	resp := gen.ListPlugins200JSONResponse{Items: make([]gen.Plugin, 0, len(all))}
	for _, p := range all {
		m, err := toAPIPlugin(p.Manifest())
		if err != nil {
			return nil, err
		}
		resp.Items = append(resp.Items, m)
	}
	return resp, nil
}

func (s *Server) GetPlugin(_ context.Context, req gen.GetPluginRequestObject) (gen.GetPluginResponseObject, error) {
	p, ok := plugin.Lookup(req.Type)
	if !ok {
		return nil, errNotFound
	}
	m, err := toAPIPlugin(p.Manifest())
	if err != nil {
		return nil, err
	}
	return gen.GetPlugin200JSONResponse(m), nil
}

func toAPIPlugin(m plugin.Manifest) (gen.Plugin, error) {
	var schema map[string]any
	if err := json.Unmarshal(m.ConfigSchema, &schema); err != nil {
		return gen.Plugin{}, err
	}
	metrics := make([]gen.MetricDef, len(m.Metrics))
	for i, d := range m.Metrics {
		metrics[i] = gen.MetricDef{Name: d.Name, Unit: d.Unit, Description: d.Description,
			Kind: gen.MetricDefKind(d.Kind), RetentionClass: d.RetentionClass}
	}
	return gen.Plugin{
		Type: m.Type, Kind: gen.PluginKind(m.Kind), Description: m.Description,
		ConfigSchema: schema, CredentialTypes: nonNilSlice(m.CredentialTypes), Metrics: metrics,
		DefaultIntervalSeconds: int(m.DefaultInterval.Seconds()), MinIntervalSeconds: int(m.MinInterval.Seconds()),
		NeedsRawSocket: m.NeedsRawSocket, BillingClass: gen.PluginBillingClass(m.BillingClass),
		WhoopsyMetric: nilIfEmpty(m.WhoopsyMetric),
	}, nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
