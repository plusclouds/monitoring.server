package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/credential"
	"github.com/plusclouds/monitoring.server/internal/extref"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/threshold"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func toAPIExternal(r *extref.Ref) *gen.ExternalRef {
	if r == nil {
		return nil
	}
	return &gen.ExternalRef{Source: r.Source, Type: r.Type, Id: r.ID}
}

func fromAPIExternal(r *gen.ExternalRefWrite) *extref.Ref {
	if r == nil {
		return nil
	}
	return &extref.Ref{Source: r.Source, Type: r.Type, ID: r.Id}
}

func toAPISite(s inventory.Site) gen.Site {
	return gen.Site{
		Id: s.ID, Name: s.Name, Country: s.Country, Timezone: s.Timezone, Address: s.Address,
		ManagedBy: s.ManagedBy, External: toAPIExternal(s.External),
		CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC(),
	}
}

func siteInput(b gen.SiteWrite) inventory.SiteInput {
	return inventory.SiteInput{Name: b.Name, Country: b.Country, Timezone: b.Timezone, Address: b.Address,
		External: fromAPIExternal(b.External)}
}

func toAPIDevice(d inventory.Device) gen.Device {
	inv := d.Inventory
	if inv == nil {
		inv = map[string]any{}
	}
	tags := d.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	st := gen.DeviceStatus{Availability: gen.AvailabilityUnmonitored, Health: gen.DeviceStatusHealthOk}
	if d.Status != nil {
		st = gen.DeviceStatus{Availability: gen.Availability(d.Status.Availability), Since: utcPtr(d.Status.Since),
			Health: gen.DeviceStatusHealth(d.Status.Health), OpenIncidents: d.Status.OpenIncidents}
	}
	out := gen.Device{
		Status: &st,
		Id:     d.ID, Name: d.Name, Address: d.Address, Type: gen.DeviceType(d.Type), Tags: tags, Notes: d.Notes,
		ParentId: d.ParentID, SiteId: d.SiteID, PhysicalPeerId: d.PhysicalPeerID, Inventory: inv,
		ManagedBy: d.ManagedBy, External: toAPIExternal(d.External),
		CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(),
	}
	withDiscovered(&out, d)
	return out
}

func deviceInput(b gen.DeviceWrite) inventory.DeviceInput {
	in := inventory.DeviceInput{
		Name: b.Name, Type: string(b.Type), Notes: b.Notes, ParentID: b.ParentId, SiteID: b.SiteId,
		PhysicalPeerID: b.PhysicalPeerId, External: fromAPIExternal(b.External),
	}
	if b.Address != nil {
		in.Address = *b.Address
	}
	if b.Tags != nil {
		in.Tags = *b.Tags
	}
	return in
}

func toAPIDependency(d inventory.Dependency) gen.Dependency {
	return gen.Dependency{DeviceId: d.DeviceID, DependsOnId: d.DependsOnID, Source: d.Source, CreatedAt: d.CreatedAt}
}

func toAPICredential(c credential.Credential) gen.Credential {
	fields := c.Fields
	if fields == nil {
		fields = map[string]string{}
	}
	return gen.Credential{
		Id: c.ID, Name: c.Name, Type: c.Type, Fields: fields, SecretsSet: nonNilSlice(c.SecretNames),
		ManagedBy: c.ManagedBy, External: toAPIExternal(c.External),
		CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC(),
	}
}

func credentialInput(b gen.CredentialWrite) credential.Input {
	in := credential.Input{Name: &b.Name, Type: b.Type, External: fromAPIExternal(b.External)}
	if b.Fields != nil {
		in.Fields = *b.Fields
	}
	return in
}

// The API's threshold rule has the same JSON shape as threshold.Rule.
func rulesFromAPI(in *[]gen.ThresholdRule) ([]threshold.Rule, error) {
	if in == nil {
		return nil, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out []threshold.Rule
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, problem(http.StatusUnprocessableEntity, "invalid-value", "Invalid value", "thresholds: "+err.Error())
	}
	return out, nil
}

func rulesToAPI(in []threshold.Rule) []gen.ThresholdRule {
	out := []gen.ThresholdRule{}
	b, _ := json.Marshal(in)
	_ = json.Unmarshal(b, &out)
	return out
}

// toAPICheck converts a check; ingestURL is the ingest listener's public
// URL ("" when not configured), for push checks.
func toAPICheck(c inventory.Check, ingestURL string) gen.Check {
	var cfg map[string]any
	_ = json.Unmarshal(c.Config, &cfg)
	if cfg == nil {
		cfg = map[string]any{}
	}
	creds := c.Credentials
	if creds == nil {
		creds = map[string]uuid.UUID{}
	}
	out := gen.Check{
		Id: c.ID, DeviceId: c.DeviceID, Name: c.Name, Plugin: c.Plugin, Config: cfg,
		IntervalSeconds: c.IntervalSeconds, TimeoutSeconds: c.TimeoutSeconds, Enabled: c.Enabled,
		Thresholds: rulesToAPI(c.Thresholds), FailureCount: c.FailureCount, RecoveryCount: c.RecoveryCount,
		IsHostCheck: c.IsHostCheck, UnknownIsCritical: c.UnknownIsCritical, RunbookUrl: c.RunbookURL,
		Credentials: creds, ManagedBy: c.ManagedBy, CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC(),
		Whoopsy: whoopsyToAPI(c.Whoopsy), Push: pushToAPI(c, ingestURL),
	}
	if c.PushToken != "" {
		out.PushToken = &c.PushToken
	}
	return out
}

func pushToAPI(c inventory.Check, ingestURL string) *gen.PushSource {
	if c.Push == nil {
		return nil
	}
	path := "/ingest/v1/" + c.ID.String()
	out := &gen.PushSource{IngestPath: path, TokenPrefix: c.Push.TokenPrefix,
		TokenCreatedAt: c.Push.TokenCreatedAt.UTC(), LastPushAt: c.Push.LastPushAt}
	if ingestURL != "" {
		u := strings.TrimSuffix(ingestURL, "/") + path
		out.IngestUrl = &u
	}
	return out
}

func whoopsyToAPI(w *threshold.Whoopsy) *gen.Whoopsy {
	if w == nil {
		return nil
	}
	out, err := via[gen.Whoopsy](w)
	if err != nil {
		return nil
	}
	return &out
}

func checkInput(b gen.CheckWrite) (inventory.CheckInput, error) {
	in := inventory.CheckInput{
		Name: b.Name, Plugin: b.Plugin, TimeoutSeconds: b.TimeoutSeconds, RunbookURL: b.RunbookUrl, Enabled: true,
	}
	if b.Config != nil {
		raw, err := json.Marshal(*b.Config)
		if err != nil {
			return in, err
		}
		in.Config = raw
	}
	deref(&in.IntervalSeconds, b.IntervalSeconds)
	deref(&in.Enabled, b.Enabled)
	deref(&in.FailureCount, b.FailureCount)
	deref(&in.RecoveryCount, b.RecoveryCount)
	deref(&in.IsHostCheck, b.IsHostCheck)
	deref(&in.UnknownIsCritical, b.UnknownIsCritical)
	if b.Credentials != nil {
		in.Credentials = *b.Credentials
	}
	var err error
	in.Thresholds, err = rulesFromAPI(b.Thresholds)
	return in, err
}

func deref[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func toAPICheckRun(c inventory.Check, r plugin.Result) gen.CheckRun {
	metrics := map[string]*float64{}
	if p, ok := plugin.Lookup(c.Plugin); ok {
		for i, d := range p.Manifest().Metrics {
			if i < len(r.Metrics) && !math.IsNaN(r.Metrics[i]) && !math.IsInf(r.Metrics[i], 0) {
				v := r.Metrics[i]
				metrics[d.Name] = &v
			} else {
				metrics[d.Name] = nil
			}
		}
	}
	run := gen.CheckRun{
		CheckId: c.ID, Name: c.Name, Plugin: c.Plugin, Status: gen.CheckRunStatus(r.Status.String()),
		Output: r.Output, DurationMs: float64(r.Duration.Microseconds()) / 1000, Metrics: metrics,
		Time: r.Time.UTC(),
	}
	if p, ok := plugin.Lookup(c.Plugin); ok && p.Manifest().Kind == plugin.KindCollector {
		run.Metrics = map[string]*float64{}
		if r.Objects != nil {
			objs := make([]map[string]any, len(r.Objects))
			for i, o := range r.Objects {
				m := map[string]*float64{}
				for j, d := range p.Manifest().Metrics {
					if j < len(o.Metrics) && !math.IsNaN(o.Metrics[j]) && !math.IsInf(o.Metrics[j], 0) {
						v := o.Metrics[j]
						m[d.Name] = &v
					} else {
						m[d.Name] = nil
					}
				}
				objs[i] = map[string]any{"key": o.Key, "name": o.Name, "labels": o.Labels,
					"status": o.Status.String(), "output": o.Output, "metrics": m}
			}
			if conv, err := via[gen.CheckRun](map[string]any{"objects": objs}); err == nil {
				run.Objects = conv.Objects
			}
		}
	}
	return run
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// withDiscovered sets out.Discovered for a device a collector discovered.
func withDiscovered(out *gen.Device, d inventory.Device) {
	if d.CollectorKey == nil {
		return
	}
	check, err := uuid.Parse(d.ManagedBy)
	if err != nil {
		return
	}
	if v, err := via[gen.Device](map[string]any{"discovered": map[string]any{"check_id": check, "key": *d.CollectorKey,
		"gone_at": utcPtr(d.CollectorGoneAt)}}); err == nil {
		out.Discovered = v.Discovered
	}
}
