package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Health{}) }

// Config of a redfish.health collector.
type Config struct {
	VerifyCertificate bool     `json:"verify_certificate,omitempty" jsonschema:"default=false,description=Verify the BMC's TLS certificate. Off by default because most BMCs ship a self-signed certificate; turn it on once the BMC has a trusted one"`
	Skip              []string `json:"skip,omitempty" jsonschema:"description=Component kinds not to report: system cpu memory storage drive volume temperature fan psu power"`
}

// Component kinds, the prefix of every object key.
var kinds = []string{"system", "cpu", "memory", "storage", "drive", "volume", "temperature", "fan", "psu", "power"}

// Metric slots, per component.
const (
	mHealth = iota
	mTemperature
	mFanRPM
	mFanPercent
	mWatts
	metricCount
)

// Health implements redfish.health: chassis thermal and power, processors,
// memory and storage of a server, each component an object with the health
// its BMC reports.
type Health struct{}

func (*Health) Manifest() plugin.Manifest {
	g := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type:            "redfish.health",
		Kind:            plugin.KindCollector,
		Description:     "Redfish (Dell iDRAC, HPE iLO, AMI-based BMCs): health of processors, memory, storage, fans, temperatures and power supplies, and power use. Each component is an object.",
		ConfigSchema:    plugin.SchemaFor[Config](),
		CredentialTypes: []string{"redfish"}, CredentialsRequired: true,
		Metrics: []plugin.MetricDef{
			g("health", "1", "0 OK, 1 warning, 2 critical, as the BMC reports it"),
			g("temperature_celsius", "Cel", "Temperature reading"),
			g("fan_rpm", "1/min", "Fan speed"),
			g("fan_percent", "percent", "Fan speed, for BMCs that report it as a percentage"),
			g("power_watts", "W", "Power: consumed by the system, or delivered by a power supply"),
		},
		DefaultInterval: 2 * time.Minute,
		MinInterval:     time.Minute,
		Slow:            true,
		PerTargetLimit:  1,
		BillingClass:    plugin.BillingStandard,
	}
}

func (*Health) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, Config{})
	if err != nil {
		return err
	}
	for _, k := range cfg.Skip {
		if !slices.Contains(kinds, k) {
			return fmt.Errorf("skip: %q is not one of %v", k, kinds)
		}
	}
	return nil
}

// Redfish resources, only the fields read.
type (
	link struct {
		ID string `json:"@odata.id"`
	}
	status struct {
		State        string
		Health       string
		HealthRollup string
	}
	collection struct{ Members []json.RawMessage }
	root       struct {
		Systems, Chassis, Managers link
	}
	system struct {
		ID           string `json:"Id"`
		Name         string
		Manufacturer string
		Model        string
		SerialNumber string
		BiosVersion  string
		PowerState   string
		Status       status
		Processors   link
		Memory       link
		Storage      link
	}
	processor struct {
		ID, Name, Model, Manufacturer string
		TotalCores                    int
		Status                        status
	}
	memory struct {
		ID, Name, Manufacturer, PartNumber, SerialNumber string
		CapacityMiB                                      int
		Status                                           status
	}
	storage struct {
		ID, Name           string
		Status             status
		StorageControllers []struct {
			MemberID string `json:"MemberId"`
			Name     string
			Model    string
			Status   status
		}
		Drives  []link
		Volumes link
	}
	drive struct {
		ID, Name, Model, SerialNumber, MediaType, Protocol string
		CapacityBytes                                      int64
		Status                                             status
	}
	volume struct {
		ID, Name, RAIDType string
		CapacityBytes      int64
		Status             status
	}
	chassis struct {
		ID           string `json:"Id"`
		Name         string
		Manufacturer string
		Model        string
		SerialNumber string
		Status       status
		Thermal      link
		Power        link
	}
	thermal struct {
		Temperatures []struct {
			MemberID                  string `json:"MemberId"`
			Name                      string
			ReadingCelsius            *float64
			UpperThresholdNonCritical *float64
			UpperThresholdCritical    *float64
			PhysicalContext           string
			Status                    status
		}
		Fans []struct {
			MemberID     string `json:"MemberId"`
			Name         string
			FanName      string // Redfish before 1.1
			Reading      *float64
			ReadingUnits string
			Status       status
		}
	}
	power struct {
		PowerControl []struct {
			MemberID           string `json:"MemberId"`
			Name               string
			PowerConsumedWatts *float64
			Status             status
		}
		PowerSupplies []struct {
			MemberID             string `json:"MemberId"`
			Name                 string
			Model                string
			SerialNumber         string
			PowerCapacityWatts   *float64
			LastPowerOutputWatts *float64
			Status               status
		}
	}
	manager struct {
		FirmwareVersion string
		Model           string
	}
)

// absent reports components that are not there: empty DIMM slots, disabled
// sensors. They are not objects.
func (s status) absent() bool {
	switch s.State {
	case "Absent", "Disabled", "UnavailableOffline":
		return true
	}
	return false
}

func (s status) health() (plugin.Status, float64) {
	h := s.Health
	if h == "" {
		h = s.HealthRollup
	}
	switch h {
	case "Warning":
		return plugin.Warning, 1
	case "Critical":
		return plugin.Critical, 2
	}
	return plugin.OK, 0
}

// collector gathers objects of one run.
type collector struct {
	c       *client
	skip    map[string]bool
	objects []plugin.Object
	multi   bool // several systems or chassis: keys carry their ID
}

func (*Health) Collect(ctx context.Context, t plugin.Target) (plugin.Batch, plugin.Inventory, error) {
	cfg, err := plugin.DecodeConfig(t.Config, Config{})
	if err != nil {
		return plugin.Batch{}, plugin.Inventory{}, err
	}
	c, err := newClient(t, !cfg.VerifyCertificate)
	if err != nil {
		return plugin.Batch{Status: plugin.Unknown, Output: err.Error()}, plugin.Inventory{}, nil
	}
	defer c.logout()
	if err := c.login(ctx); err != nil {
		return failed(c, err), plugin.Inventory{}, nil
	}
	col := &collector{c: c, skip: map[string]bool{}}
	for _, k := range cfg.Skip {
		col.skip[k] = true
	}
	inv, err := col.run(ctx)
	if err != nil {
		return failed(c, err), plugin.Inventory{}, nil
	}
	if col.objects == nil {
		col.objects = []plugin.Object{}
	}
	return plugin.Batch{Status: plugin.OK, Output: summary(inv, col.objects), Objects: col.objects},
		plugin.Inventory{Device: &inv}, nil
}

// failed turns an error into a failed collection: no answer is CRITICAL,
// anything the BMC answered wrongly is UNKNOWN.
func failed(c *client, err error) plugin.Batch {
	var pe *plugin.PolicyError
	var ae *authError
	switch {
	case errors.As(err, &pe):
		return plugin.Batch{Status: plugin.Unknown, Output: pe.Error()}
	case errors.As(err, &ae):
		return plugin.Batch{Status: plugin.Unknown, Output: ae.Error()}
	case certificateError(err):
		return plugin.Batch{Status: plugin.Unknown, Output: fmt.Sprintf(
			"the certificate of %s is not trusted (%v): install a trusted certificate on the BMC or turn verify_certificate off", c.base.Host, err)}
	case unreachable(err):
		return plugin.Batch{Status: plugin.Critical, Output: fmt.Sprintf("BMC %s does not answer: %v", c.base.Host, err)}
	}
	return plugin.Batch{Status: plugin.Unknown, Output: fmt.Sprintf("BMC %s: %v", c.base.Host, err)}
}

func (col *collector) run(ctx context.Context) (plugin.DeviceInfo, error) {
	var inv plugin.DeviceInfo
	var r root
	if err := col.c.get(ctx, "/redfish/v1/", &r); err != nil {
		return inv, err
	}
	systems, err := members[system](ctx, col.c, r.Systems.ID)
	if err != nil {
		return inv, err
	}
	chassisList, err := members[chassis](ctx, col.c, r.Chassis.ID)
	if err != nil {
		return inv, err
	}
	// Enclosures list sub-chassis without sensors; keep those that have them.
	chassisList = slices.DeleteFunc(chassisList, func(ch chassis) bool { return ch.Thermal.ID == "" && ch.Power.ID == "" })
	col.multi = len(systems) > 1 || len(chassisList) > 1

	for _, s := range systems {
		if inv.Vendor == "" {
			inv = plugin.DeviceInfo{Vendor: s.Manufacturer, Model: s.Model, Serial: s.SerialNumber, Firmware: s.BiosVersion}
		}
		if err := col.system(ctx, s); err != nil {
			return inv, err
		}
	}
	for _, ch := range chassisList {
		if inv.Vendor == "" {
			inv = plugin.DeviceInfo{Vendor: ch.Manufacturer, Model: ch.Model, Serial: ch.SerialNumber}
		}
		if err := col.chassis(ctx, ch); err != nil {
			return inv, err
		}
	}
	if managers, err := members[manager](ctx, col.c, r.Managers.ID); err == nil && len(managers) > 0 && len(col.objects) > 0 {
		for i := range col.objects {
			if strings.HasPrefix(col.objects[i].Key, "system") {
				col.objects[i].Labels["bmc_firmware"] = managers[0].FirmwareVersion
			}
		}
	}
	return inv, nil
}

func (col *collector) key(kind, scope, id string) string {
	if col.multi && scope != "" {
		return kind + ":" + scope + ":" + id
	}
	return kind + ":" + id
}

func (col *collector) add(kind, key, name string, st status, labels map[string]string, set func([]float64)) {
	if col.skip[kind] || st.absent() {
		return
	}
	s, h := st.health()
	o := plugin.Object{Key: key, Name: name, Status: s, Metrics: plugin.NaNs(metricCount), Labels: map[string]string{"kind": kind}}
	for k, v := range labels {
		if v != "" {
			o.Labels[k] = v
		}
	}
	o.Metrics[mHealth] = h
	if set != nil {
		set(o.Metrics)
	}
	o.Output = describe(st, o)
	col.objects = append(col.objects, o)
}

func describe(st status, o plugin.Object) string {
	health := st.Health
	if health == "" {
		health = st.HealthRollup
	}
	if health == "" {
		health = "no health reported"
	}
	parts := []string{health}
	if v := o.Metrics[mTemperature]; !math.IsNaN(v) {
		parts = append(parts, strconv.FormatFloat(v, 'f', -1, 64)+" °C")
	}
	if v := o.Metrics[mFanRPM]; !math.IsNaN(v) {
		parts = append(parts, strconv.FormatFloat(v, 'f', -1, 64)+" RPM")
	}
	if v := o.Metrics[mFanPercent]; !math.IsNaN(v) {
		parts = append(parts, strconv.FormatFloat(v, 'f', -1, 64)+" %")
	}
	if v := o.Metrics[mWatts]; !math.IsNaN(v) {
		parts = append(parts, strconv.FormatFloat(v, 'f', -1, 64)+" W")
	}
	return strings.Join(parts, ", ")
}

func (col *collector) system(ctx context.Context, s system) error {
	name := s.Name
	if s.Model != "" {
		name = strings.TrimSpace(s.Manufacturer + " " + s.Model)
	}
	col.add("system", col.key("system", "", s.ID), name, s.Status, map[string]string{
		"vendor": s.Manufacturer, "model": s.Model, "serial": s.SerialNumber, "bios": s.BiosVersion, "power_state": s.PowerState,
	}, nil)
	if !col.skip["cpu"] {
		cpus, err := optional(members[processor](ctx, col.c, s.Processors.ID))
		if err != nil {
			return err
		}
		for _, p := range cpus {
			col.add("cpu", col.key("cpu", s.ID, p.ID), nameOr(p.Name, p.ID), p.Status,
				map[string]string{"model": p.Model, "cores": itoa(p.TotalCores)}, nil)
		}
	}
	if !col.skip["memory"] {
		dimms, err := optional(members[memory](ctx, col.c, s.Memory.ID))
		if err != nil {
			return err
		}
		for _, m := range dimms {
			col.add("memory", col.key("memory", s.ID, m.ID), nameOr(m.Name, m.ID), m.Status, map[string]string{
				"capacity": mib(m.CapacityMiB), "part": m.PartNumber, "serial": m.SerialNumber, "vendor": m.Manufacturer,
			}, nil)
		}
	}
	if col.skip["storage"] && col.skip["drive"] && col.skip["volume"] {
		return nil
	}
	stores, err := optional(members[storage](ctx, col.c, s.Storage.ID))
	if err != nil {
		return err
	}
	for _, st := range stores {
		for i, ctl := range st.StorageControllers {
			id := st.ID
			if len(st.StorageControllers) > 1 {
				id += "." + nameOr(ctl.MemberID, strconv.Itoa(i))
			}
			col.add("storage", col.key("storage", s.ID, id), nameOr(ctl.Name, nameOr(ctl.Model, st.Name)), ctl.Status,
				map[string]string{"model": ctl.Model}, nil)
		}
		if len(st.StorageControllers) == 0 {
			col.add("storage", col.key("storage", s.ID, st.ID), nameOr(st.Name, st.ID), st.Status, nil, nil)
		}
		if !col.skip["drive"] {
			for _, l := range st.Drives {
				var d drive
				if err := col.c.get(ctx, l.ID, &d); err != nil {
					if notFound(err) {
						continue
					}
					return err
				}
				col.add("drive", col.key("drive", s.ID, d.ID), nameOr(d.Name, d.ID), d.Status, map[string]string{
					"model": d.Model, "serial": d.SerialNumber, "media": d.MediaType, "protocol": d.Protocol, "capacity": bytesText(d.CapacityBytes),
				}, nil)
			}
		}
		if !col.skip["volume"] && st.Volumes.ID != "" {
			vols, err := optional(members[volume](ctx, col.c, st.Volumes.ID))
			if err != nil {
				return err
			}
			for _, v := range vols {
				col.add("volume", col.key("volume", s.ID, v.ID), nameOr(v.Name, v.ID), v.Status,
					map[string]string{"raid": v.RAIDType, "capacity": bytesText(v.CapacityBytes)}, nil)
			}
		}
	}
	return nil
}

func (col *collector) chassis(ctx context.Context, ch chassis) error {
	if ch.Thermal.ID != "" && (!col.skip["temperature"] || !col.skip["fan"]) {
		var th thermal
		if err := col.c.get(ctx, ch.Thermal.ID, &th); err != nil && !notFound(err) {
			return err
		}
		for i, t := range th.Temperatures {
			id := nameOr(t.Name, nameOr(t.MemberID, strconv.Itoa(i)))
			st := t.Status
			if st.Health == "" && t.ReadingCelsius != nil { // no health: judge by the BMC's own thresholds
				switch r := *t.ReadingCelsius; {
				case t.UpperThresholdCritical != nil && r >= *t.UpperThresholdCritical:
					st.Health = "Critical"
				case t.UpperThresholdNonCritical != nil && r >= *t.UpperThresholdNonCritical:
					st.Health = "Warning"
				}
			}
			col.add("temperature", col.key("temperature", ch.ID, id), id, st,
				map[string]string{"context": t.PhysicalContext, "warning_at": num(t.UpperThresholdNonCritical), "critical_at": num(t.UpperThresholdCritical)},
				func(m []float64) { setPtr(m, mTemperature, t.ReadingCelsius) })
		}
		for i, f := range th.Fans {
			id := nameOr(f.Name, nameOr(f.FanName, nameOr(f.MemberID, strconv.Itoa(i))))
			col.add("fan", col.key("fan", ch.ID, id), id, f.Status, nil, func(m []float64) {
				if strings.EqualFold(f.ReadingUnits, "Percent") {
					setPtr(m, mFanPercent, f.Reading)
				} else {
					setPtr(m, mFanRPM, f.Reading)
				}
			})
		}
	}
	if ch.Power.ID != "" && (!col.skip["psu"] || !col.skip["power"]) {
		var pw power
		if err := col.c.get(ctx, ch.Power.ID, &pw); err != nil && !notFound(err) {
			return err
		}
		for i, p := range pw.PowerControl {
			id := nameOr(p.MemberID, strconv.Itoa(i))
			if p.PowerConsumedWatts == nil {
				continue
			}
			col.add("power", col.key("power", ch.ID, id), nameOr(p.Name, "Power"), p.Status, nil,
				func(m []float64) { setPtr(m, mWatts, p.PowerConsumedWatts) })
		}
		for i, p := range pw.PowerSupplies {
			id := nameOr(p.Name, nameOr(p.MemberID, strconv.Itoa(i)))
			if p.MemberID != "" {
				id = "PSU " + p.MemberID
			}
			col.add("psu", col.key("psu", ch.ID, id), nameOr(p.Name, id), p.Status,
				map[string]string{"model": p.Model, "serial": p.SerialNumber, "capacity_watts": num(p.PowerCapacityWatts)},
				func(m []float64) { setPtr(m, mWatts, p.LastPowerOutputWatts) })
		}
	}
	return nil
}

// members reads a collection and its members. Members already inline (a
// BMC that expands collections) are used as they are; the others are read
// in parallel, a few at a time.
func members[T any](ctx context.Context, c *client, path string) ([]T, error) {
	if path == "" {
		return nil, nil
	}
	var col collection
	if err := c.get(ctx, path, &col); err != nil {
		return nil, err
	}
	out := make([]T, len(col.Members))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(2)
	for i, raw := range col.Members {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, err
		}
		if len(probe) > 2 { // more than @odata.id: expanded
			if err := json.Unmarshal(raw, &out[i]); err != nil {
				return nil, err
			}
			continue
		}
		var l link
		if err := json.Unmarshal(raw, &l); err != nil || l.ID == "" {
			continue
		}
		g.Go(func() error { return c.get(gctx, l.ID, &out[i]) })
	}
	return out, g.Wait()
}

// optional treats a missing collection as empty.
func optional[T any](v []T, err error) ([]T, error) {
	if notFound(err) {
		return nil, nil
	}
	return v, err
}

func summary(inv plugin.DeviceInfo, objs []plugin.Object) string {
	var warn, crit []string
	for _, o := range objs {
		switch o.Status {
		case plugin.Warning:
			warn = append(warn, o.Name)
		case plugin.Critical:
			crit = append(crit, o.Name)
		}
	}
	head := strings.TrimSpace(inv.Vendor + " " + inv.Model)
	if inv.Serial != "" {
		head += " (serial " + inv.Serial + ")"
	}
	if head == "" {
		head = "BMC"
	}
	out := fmt.Sprintf("%s: %d components", head, len(objs))
	switch {
	case len(crit) == 0 && len(warn) == 0:
		out += ", all OK"
	default:
		if len(crit) > 0 {
			out += fmt.Sprintf(", %d critical (%s)", len(crit), short(crit))
		}
		if len(warn) > 0 {
			out += fmt.Sprintf(", %d warning (%s)", len(warn), short(warn))
		}
	}
	return out
}

func short(names []string) string {
	if len(names) > 5 {
		return strings.Join(names[:5], ", ") + ", ..."
	}
	return strings.Join(names, ", ")
}

func setPtr(m []float64, slot int, v *float64) {
	if v != nil {
		m[slot] = *v
	}
}

func nameOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return strings.TrimSpace(s)
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func num(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func mib(n int) string {
	switch {
	case n == 0:
		return ""
	case n%1024 == 0:
		return strconv.Itoa(n/1024) + " GiB"
	}
	return strconv.Itoa(n) + " MiB"
}

func bytesText(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1e12:
		return strconv.FormatFloat(float64(n)/1e12, 'f', 2, 64) + " TB"
	}
	return strconv.FormatFloat(float64(n)/1e9, 'f', 0, 64) + " GB"
}
