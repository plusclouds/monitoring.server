package snmp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&System{}) }

// SystemConfig of an snmp.system check.
type SystemConfig struct {
	Connection
	Profile string `json:"profile,omitempty" jsonschema:"enum=auto,enum=host-resources,enum=net-snmp,enum=cisco,enum=juniper,enum=mikrotik,enum=fortinet,enum=hpe,default=auto,description=Where CPU and memory are read. auto picks the profile from sysObjectID and falls back to HOST-RESOURCES-MIB"`
}

var systemDefaults = SystemConfig{Profile: "auto"}

// Metric slots of snmp.system.
const (
	sUptime = iota
	sCPU
	sMemory
	systemMetrics
)

// System implements the snmp.system check: uptime, CPU and memory.
type System struct{}

func (*System) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type:            "snmp.system",
		Kind:            plugin.KindCheck,
		Description:     "SNMP: uptime, CPU and memory use, from a vendor profile or HOST-RESOURCES-MIB. Reports restarts.",
		ConfigSchema:    plugin.SchemaFor[SystemConfig](),
		CredentialTypes: credentialTypes,
		Metrics: []plugin.MetricDef{
			{Name: "uptime_seconds", Unit: "s", Description: "System uptime (hrSystemUptime, else sysUpTime)", Kind: "gauge", RetentionClass: plugin.RetentionStandard},
			{Name: "cpu_percent", Unit: "percent", Description: "CPU use: average of all processors, or the busiest module on chassis devices", Kind: "gauge", RetentionClass: plugin.RetentionStandard},
			{Name: "memory_used_percent", Unit: "percent", Description: "Memory in use", Kind: "gauge", RetentionClass: plugin.RetentionStandard},
		},
		DefaultInterval: time.Minute,
		MinInterval:     30 * time.Second,
		BillingClass:    plugin.BillingStandard,
	}
}

func (*System) Validate(raw json.RawMessage) error {
	_, err := plugin.DecodeConfig(raw, systemDefaults)
	return err
}

func (*System) Run(ctx context.Context, t plugin.Target) (plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(t.Config, systemDefaults)
	if err != nil {
		return plugin.Result{}, err
	}
	s, err := open(ctx, t, cfg.Connection)
	if err != nil {
		return plugin.Result{Status: plugin.Unknown, Output: err.Error()}, nil
	}
	defer s.close()

	vals, err := s.get(oidSysObjectID, oidSysUpTime, oidHrSystemUptime, oidSysName)
	if err != nil {
		return s.failure(err), nil
	}
	now := time.Now()
	r := plugin.Result{Metrics: plugin.NaNs(systemMetrics), Time: now.UTC()}
	var notes []string

	uptime, hasUptime := uptimeSeconds(vals)
	if hasUptime {
		r.Metrics[sUptime] = uptime
		if prev, ok := loadSample(t.State, "uptime"); ok && restarted(prev.Value, uptime, now.Sub(prev.At).Seconds()) {
			notes = append(notes, fmt.Sprintf("restarted %s ago", human(uptime)))
		}
		saveSample(t.State, "uptime", sample{Value: uptime, At: now})
	}

	objectID := ""
	if v, ok := vals[oidSysObjectID]; ok {
		objectID = text(v)
	}
	p := pickProfile(cfg.Profile, objectID)
	used := p.name
	cpu, okCPU, err := p.cpu(s)
	if err != nil {
		return s.failure(err), nil
	}
	mem, okMem, err := p.memory(s)
	if err != nil {
		return s.failure(err), nil
	}
	// Vendor tables missing: HOST-RESOURCES-MIB is the common fallback.
	if (!okCPU || !okMem) && p.name != hostResources.name {
		if !okCPU {
			if cpu, okCPU, err = hostResources.cpu(s); err != nil {
				return s.failure(err), nil
			}
		}
		if !okMem {
			if mem, okMem, err = hostResources.memory(s); err != nil {
				return s.failure(err), nil
			}
		}
		used += "+host-resources"
	}
	if okCPU {
		r.Metrics[sCPU] = cpu
	}
	if okMem {
		r.Metrics[sMemory] = mem
	}

	name := s.addr
	if v, ok := vals[oidSysName]; ok && text(v) != "" {
		name = text(v)
	}
	parts := []string{name + ":"}
	if hasUptime {
		parts = append(parts, "up "+human(uptime)+",")
	}
	parts = append(parts, "CPU "+pct(r.Metrics[sCPU], okCPU)+",", "memory "+pct(r.Metrics[sMemory], okMem))
	parts = append(parts, "(profile "+used+")")
	r.Output = strings.Join(parts, " ")
	if len(notes) > 0 {
		r.Output += "; " + strings.Join(notes, "; ")
	}
	if !hasUptime && !okCPU && !okMem {
		r.Status = plugin.Unknown
		r.Output = fmt.Sprintf("%s answered, but has none of the uptime, CPU or memory objects", name)
	}
	return r, nil
}

func pct(v float64, ok bool) string {
	if !ok {
		return "n/a"
	}
	return fmtNum(v) + " %"
}

// human prints a duration in seconds as days, hours and minutes.
func human(sec float64) string {
	d := time.Duration(sec) * time.Second
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// A profile reads CPU and memory for one vendor. A reader returns false
// when the device does not have the objects; an error only for a failed
// request.
type profile struct {
	name   string
	prefix string // sysObjectID enterprise prefix for auto-detection
	cpu    func(*session) (float64, bool, error)
	memory func(*session) (float64, bool, error)
}

var hostResources = profile{
	name: "host-resources",
	// hrProcessorLoad: one row per processor (core), percent over the last minute.
	cpu:    func(s *session) (float64, bool, error) { return column(s, ".1.3.6.1.2.1.25.3.3.1.2", mean) },
	memory: hrMemory,
}

var profiles = []profile{
	hostResources,
	{
		name: "net-snmp", prefix: ".1.3.6.1.4.1.8072",
		cpu: hostResources.cpu,
		// UCD-SNMP-MIB: total minus available, buffers and cache. hrStorage
		// "Physical memory" counts the page cache as used.
		memory: func(s *session) (float64, bool, error) {
			v, err := s.get(".1.3.6.1.4.1.2021.4.5.0", ".1.3.6.1.4.1.2021.4.6.0", ".1.3.6.1.4.1.2021.4.14.0", ".1.3.6.1.4.1.2021.4.15.0")
			if err != nil {
				return 0, false, err
			}
			total, ok1 := num(v, ".1.3.6.1.4.1.2021.4.5.0")
			avail, ok2 := num(v, ".1.3.6.1.4.1.2021.4.6.0")
			if !ok1 || !ok2 || total <= 0 {
				return 0, false, nil
			}
			buffers, _ := num(v, ".1.3.6.1.4.1.2021.4.14.0")
			cached, _ := num(v, ".1.3.6.1.4.1.2021.4.15.0")
			return clamp((total - avail - buffers - cached) / total * 100), true, nil
		},
	},
	{
		name: "cisco", prefix: ".1.3.6.1.4.1.9",
		// CISCO-PROCESS-MIB cpmCPUTotal1minRev, per CPU or module.
		cpu: func(s *session) (float64, bool, error) { return column(s, ".1.3.6.1.4.1.9.9.109.1.1.1.1.7", maximum) },
		memory: func(s *session) (float64, bool, error) {
			// cpmCPUMemoryUsed and Free (KB), else CISCO-MEMORY-POOL-MIB pool 1 (processor).
			if p, ok, err := usedFree(s, ".1.3.6.1.4.1.9.9.109.1.1.1.1.12", ".1.3.6.1.4.1.9.9.109.1.1.1.1.13"); err != nil || ok {
				return p, ok, err
			}
			v, err := s.get(".1.3.6.1.4.1.9.9.48.1.1.1.5.1", ".1.3.6.1.4.1.9.9.48.1.1.1.6.1")
			if err != nil {
				return 0, false, err
			}
			used, ok1 := num(v, ".1.3.6.1.4.1.9.9.48.1.1.1.5.1")
			free, ok2 := num(v, ".1.3.6.1.4.1.9.9.48.1.1.1.6.1")
			if !ok1 || !ok2 || used+free <= 0 {
				return 0, false, nil
			}
			return clamp(used / (used + free) * 100), true, nil
		},
	},
	{
		name: "juniper", prefix: ".1.3.6.1.4.1.2636",
		// JUNIPER-MIB jnxOperatingCPU and jnxOperatingBuffer, per component;
		// the routing engines are the busiest rows.
		cpu:    func(s *session) (float64, bool, error) { return column(s, ".1.3.6.1.4.1.2636.3.1.13.1.8", maximum) },
		memory: func(s *session) (float64, bool, error) { return column(s, ".1.3.6.1.4.1.2636.3.1.13.1.11", maximum) },
	},
	{
		// RouterOS answers HOST-RESOURCES-MIB for CPU and memory.
		name: "mikrotik", prefix: ".1.3.6.1.4.1.14988",
		cpu: hostResources.cpu, memory: hostResources.memory,
	},
	{
		name: "fortinet", prefix: ".1.3.6.1.4.1.12356",
		// FORTINET-FORTIGATE-MIB fgSysCpuUsage and fgSysMemUsage (percent).
		cpu:    func(s *session) (float64, bool, error) { return scalar(s, ".1.3.6.1.4.1.12356.101.4.1.3.0") },
		memory: func(s *session) (float64, bool, error) { return scalar(s, ".1.3.6.1.4.1.12356.101.4.1.4.0") },
	},
	{
		// HP/Aruba ArubaOS-Switch (ProCurve). Aruba CX uses HOST-RESOURCES.
		name: "hpe", prefix: ".1.3.6.1.4.1.11",
		cpu: func(s *session) (float64, bool, error) { return scalar(s, ".1.3.6.1.4.1.11.2.14.11.5.1.9.6.1.0") },
		// hpLocalMemTotalBytes and hpLocalMemFreeBytes, per slot.
		memory: func(s *session) (float64, bool, error) {
			total, err := s.walk(".1.3.6.1.4.1.11.2.14.11.5.1.1.2.1.1.1.5")
			if err != nil || len(total) == 0 {
				return 0, false, err
			}
			free, err := s.walk(".1.3.6.1.4.1.11.2.14.11.5.1.1.2.1.1.1.6")
			if err != nil || len(free) == 0 {
				return 0, false, err
			}
			t, _ := number(total[0])
			f, _ := number(free[0])
			if t <= 0 {
				return 0, false, nil
			}
			return clamp((t - f) / t * 100), true, nil
		},
	},
}

func pickProfile(name, objectID string) profile {
	if name != "" && name != "auto" {
		for _, p := range profiles {
			if p.name == name {
				return p
			}
		}
	}
	best := hostResources
	for _, p := range profiles {
		// The longest matching prefix wins; a prefix matches whole arcs only.
		if p.prefix != "" && (objectID == p.prefix || strings.HasPrefix(objectID, p.prefix+".")) &&
			len(p.prefix) > len(best.prefix) {
			best = p
		}
	}
	return best
}

// hrMemory reads the hrStorageRam row of hrStorageTable.
func hrMemory(s *session) (float64, bool, error) {
	types, err := s.walk(".1.3.6.1.2.1.25.2.3.1.2")
	if err != nil {
		return 0, false, err
	}
	i := slices.IndexFunc(types, func(p gosnmp.SnmpPDU) bool { return text(p) == ".1.3.6.1.2.1.25.2.1.2" })
	if i < 0 {
		return 0, false, nil
	}
	idx := types[i].Name[strings.LastIndex(types[i].Name, ".")+1:]
	size, used := ".1.3.6.1.2.1.25.2.3.1.5."+idx, ".1.3.6.1.2.1.25.2.3.1.6."+idx
	v, err := s.get(size, used)
	if err != nil {
		return 0, false, err
	}
	sz, ok1 := num(v, size)
	u, ok2 := num(v, used)
	if !ok1 || !ok2 || sz <= 0 {
		return 0, false, nil
	}
	return clamp(u / sz * 100), true, nil
}

// usedFree reads two columns of used and free amounts and returns the used
// share of the first row.
func usedFree(s *session, usedOID, freeOID string) (float64, bool, error) {
	used, err := s.walk(usedOID)
	if err != nil || len(used) == 0 {
		return 0, false, err
	}
	free, err := s.walk(freeOID)
	if err != nil || len(free) == 0 {
		return 0, false, err
	}
	u, _ := number(used[0])
	f, _ := number(free[0])
	if u+f <= 0 {
		return 0, false, nil
	}
	return clamp(u / (u + f) * 100), true, nil
}

func scalar(s *session, oid string) (float64, bool, error) {
	v, err := s.get(oid)
	if err != nil {
		return 0, false, err
	}
	f, ok := num(v, oid)
	return f, ok, nil
}

func column(s *session, oid string, agg func([]float64) float64) (float64, bool, error) {
	pdus, err := s.walk(oid)
	if err != nil {
		return 0, false, err
	}
	var vals []float64
	for _, p := range pdus {
		if f, ok := number(p); ok {
			vals = append(vals, f)
		}
	}
	if len(vals) == 0 {
		return 0, false, nil
	}
	return agg(vals), true, nil
}

func num(vals map[string]gosnmp.SnmpPDU, oid string) (float64, bool) {
	v, ok := vals[oid]
	if !ok {
		return 0, false
	}
	return number(v)
}

func mean(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func maximum(v []float64) float64 { return slices.Max(v) }

func clamp(p float64) float64 { return min(max(p, 0), 100) }
