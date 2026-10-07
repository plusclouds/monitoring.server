package xapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Pool{}) }

// Config of an xapi.pool collector.
type Config struct {
	VerifyCertificate bool   `json:"verify_certificate,omitempty" jsonschema:"default=false,description=Verify the hosts' TLS certificates. Off by default because XCP-ng and XenServer hosts ship self-signed ones"`
	Performance       *bool  `json:"performance,omitempty" jsonschema:"default=true,description=Read CPU, memory, network and disk use from every host (rrd_updates)"`
	HaltedStatus      string `json:"halted_status,omitempty" jsonschema:"enum=ok,enum=warning,enum=critical,default=ok,description=Status of a VM that is halted or suspended"`
}

var defaults = Config{HaltedStatus: "ok"}

// Metric slots, per object (host, VM, storage repository).
const (
	mCPU = iota
	mMemory
	mNetRx
	mNetTx
	mDiskRead
	mDiskWrite
	mDiskIOPS
	mUsed
	mSize
	mSnapshots
	mSnapshotAge
	metricCount
)

// Pool implements xapi.pool.
type Pool struct{}

func (*Pool) Manifest() plugin.Manifest {
	g := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	r := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "rate", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type: "xapi.pool",
		Kind: plugin.KindCollector,
		Description: "XCP-ng / XenServer pool through its master's API: hosts and VMs become child devices (VMs follow migrations); " +
			"hosts, VMs and storage repositories are objects with their state, CPU, memory, network, disk and snapshot metrics.",
		ConfigSchema:    plugin.SchemaFor[Config](),
		CredentialTypes: []string{"xapi"}, CredentialsRequired: true,
		Metrics: []plugin.MetricDef{
			g("cpu_percent", "percent", "CPU use (hosts and VMs)"),
			g("memory_used_percent", "percent", "Memory in use (hosts; VMs with guest tools)"),
			r("net_rx_bps", "bit/s", "Network received (host physical interfaces, VM interfaces)"),
			r("net_tx_bps", "bit/s", "Network sent"),
			r("disk_read_bps", "byte/s", "VM disk reads"),
			r("disk_write_bps", "byte/s", "VM disk writes"),
			r("disk_iops", "1/s", "VM disk operations"),
			g("used_percent", "percent", "Storage repository use"),
			g("size_bytes", "By", "Storage repository size"),
			g("snapshot_count", "1", "Snapshots of a VM"),
			g("oldest_snapshot_days", "d", "Age of a VM's oldest snapshot"),
		},
		DefaultInterval: time.Minute,
		MinInterval:     time.Minute,
		Slow:            true,
		PerTargetLimit:  1,
		BillingClass:    plugin.BillingStandard,
		// Each hypervisor host bills, not the pool: 2 hosts bill twice
		// what 1 does. VMs and storage repositories are not billed.
		BillableObjects: map[string]string{"host:": "host"},
	}
}

func (*Pool) Validate(raw json.RawMessage) error {
	_, err := plugin.DecodeConfig(raw, defaults)
	return err
}

// API records, only the fields read.
type (
	poolRecord struct {
		NameLabel string `json:"name_label"`
		Master    string `json:"master"`
		HAEnabled bool   `json:"ha_enabled"`
	}
	hostRecord struct {
		UUID            string            `json:"uuid"`
		NameLabel       string            `json:"name_label"`
		Hostname        string            `json:"hostname"`
		Address         string            `json:"address"`
		Enabled         bool              `json:"enabled"`
		Metrics         string            `json:"metrics"`
		SoftwareVersion map[string]string `json:"software_version"`
	}
	hostMetrics struct {
		Live        bool `json:"live"`
		MemoryTotal xint `json:"memory_total"`
		MemoryFree  xint `json:"memory_free"`
	}
	vmRecord struct {
		UUID            string   `json:"uuid"`
		NameLabel       string   `json:"name_label"`
		PowerState      string   `json:"power_state"`
		IsATemplate     bool     `json:"is_a_template"`
		IsASnapshot     bool     `json:"is_a_snapshot"`
		IsControlDomain bool     `json:"is_control_domain"`
		ResidentOn      string   `json:"resident_on"`
		GuestMetrics    string   `json:"guest_metrics"`
		Snapshots       []string `json:"snapshots"`
		SnapshotTime    string   `json:"snapshot_time"`
		VCPUsMax        xint     `json:"VCPUs_max"`
		MemoryStaticMax xint     `json:"memory_static_max"`
	}
	guestMetrics struct {
		OSVersion         map[string]string `json:"os_version"`
		PVDriversVersion  map[string]string `json:"PV_drivers_version"`
		PVDriversDetected bool              `json:"PV_drivers_detected"`
	}
	srRecord struct {
		UUID                string `json:"uuid"`
		NameLabel           string `json:"name_label"`
		Type                string `json:"type"`
		Shared              bool   `json:"shared"`
		PhysicalSize        xint   `json:"physical_size"`
		PhysicalUtilisation xint   `json:"physical_utilisation"`
	}
)

const nullRef = "OpaqueRef:NULL"

func (*Pool) Collect(ctx context.Context, t plugin.Target) (plugin.Batch, plugin.Inventory, error) {
	cfg, err := plugin.DecodeConfig(t.Config, defaults)
	if err != nil {
		return plugin.Batch{}, plugin.Inventory{}, err
	}
	if t.Address == "" {
		return plugin.Batch{Status: plugin.Unknown, Output: "the device has no address"}, plugin.Inventory{}, nil
	}
	c, err := login(ctx, t, strings.TrimPrefix(t.Address, "https://"), cfg.VerifyCertificate)
	if err != nil {
		return failed(t.Address, err), plugin.Inventory{}, nil
	}
	defer c.logout()

	var (
		pools   map[string]poolRecord
		hosts   map[string]hostRecord
		hmetric map[string]hostMetrics
		vms     map[string]vmRecord
		guests  map[string]guestMetrics
		srs     map[string]srRecord
	)
	for _, r := range []struct {
		class string
		out   any
	}{{"pool", &pools}, {"host", &hosts}, {"host_metrics", &hmetric}, {"VM", &vms}, {"VM_guest_metrics", &guests}, {"SR", &srs}} {
		if err := c.records(ctx, r.class, r.out); err != nil {
			return failed(t.Address, err), plugin.Inventory{}, nil
		}
	}
	var pool poolRecord
	for _, p := range pools {
		pool = p
	}

	// Performance data, host by host: each host serves its own and its VMs'.
	perf := map[string]map[string]float64{}
	var perfErrs []string
	if cfg.Performance == nil || *cfg.Performance {
		since := time.Now().Add(-3 * time.Minute)
		for _, ref := range sortedKeys(hosts) {
			h := hosts[ref]
			if !hmetric[h.Metrics].Live || h.Address == "" {
				continue
			}
			addr := hostAddress(h.Address, c.host)
			u, err := c.rrd(ctx, t, addr, cfg.VerifyCertificate, since)
			if err != nil {
				perfErrs = append(perfErrs, fmt.Sprintf("%s: %v", h.NameLabel, err))
				continue
			}
			for k, v := range u.latest() {
				perf[k] = v
			}
		}
	}

	var inv plugin.Inventory
	inv.Children = []plugin.ChildDevice{}
	var objects []plugin.Object
	hostKey := map[string]string{} // host ref -> child key
	down := []string{}
	for _, ref := range sortedKeys(hosts) {
		h := hosts[ref]
		key := "host:" + h.UUID
		hostKey[ref] = key
		hm := hmetric[h.Metrics]
		inv.Children = append(inv.Children, plugin.ChildDevice{Key: key, Name: nameOr(h.NameLabel, h.Hostname), Type: "hypervisor_host",
			Address: h.Address, Info: plugin.DeviceInfo{Vendor: h.SoftwareVersion["product_brand"], Firmware: h.SoftwareVersion["product_version"]}})
		o := plugin.Object{Key: key, Name: nameOr(h.NameLabel, h.Hostname), Device: key, Availability: true, Metrics: plugin.NaNs(metricCount),
			Labels: map[string]string{"kind": "host", "address": h.Address, "version": h.SoftwareVersion["product_version"],
				"master": fmt.Sprint(ref == pool.Master)}}
		switch {
		case !hm.Live:
			o.Status, o.Output = plugin.Critical, "not live: the host does not answer the pool"
			down = append(down, o.Name)
		case !h.Enabled:
			o.Status, o.Output = plugin.Warning, "disabled (maintenance mode)"
		default:
			o.Output = "live"
		}
		if hm.MemoryTotal > 0 {
			o.Metrics[mMemory] = float64(hm.MemoryTotal-hm.MemoryFree) / float64(hm.MemoryTotal) * 100
		}
		if p := perf[key]; p != nil {
			if v, ok := p["cpu_avg"]; ok {
				o.Metrics[mCPU] = v * 100
			}
			o.Metrics[mNetRx], o.Metrics[mNetTx] = sum(p, pifRx)*8, sum(p, pifTx)*8
		}
		objects = append(objects, o)
	}

	running, total := 0, 0
	for _, ref := range sortedKeys(vms) {
		v := vms[ref]
		if v.IsATemplate || v.IsASnapshot || v.IsControlDomain {
			continue
		}
		total++
		key := "vm:" + v.UUID
		child := plugin.ChildDevice{Key: key, Name: nameOr(v.NameLabel, v.UUID), Type: "vm", ParentKey: hostKey[v.ResidentOn]}
		g, hasTools := guests[v.GuestMetrics]
		hasTools = hasTools && v.GuestMetrics != nullRef && (g.PVDriversDetected || len(g.PVDriversVersion) > 0)
		if os := g.OSVersion["name"]; os != "" {
			child.Info.Model = os
		}
		inv.Children = append(inv.Children, child)
		o := plugin.Object{Key: key, Name: child.Name, Device: key, Availability: true, Metrics: plugin.NaNs(metricCount),
			Labels: map[string]string{"kind": "vm", "power_state": v.PowerState, "guest_tools": fmt.Sprint(hasTools),
				"vcpus": fmt.Sprint(int64(v.VCPUsMax))}}
		switch v.PowerState {
		case "Running":
			running++
			o.Output = "running"
			if !hasTools {
				o.Output += ", no guest tools"
			}
		case "Paused":
			o.Status, o.Output = plugin.Warning, "paused"
		default:
			o.Status, o.Output = levelStatus(cfg.HaltedStatus), strings.ToLower(v.PowerState)
		}
		if p := perf[key]; p != nil && v.PowerState == "Running" {
			if cpu := mean(p, vmCPU); !math.IsNaN(cpu) {
				o.Metrics[mCPU] = cpu * 100
			}
			if mem, ok := p["memory"]; ok && mem > 0 {
				if free, ok := p["memory_internal_free"]; ok {
					o.Metrics[mMemory] = math.Max(0, (mem-free*1024)/mem*100)
				}
			}
			o.Metrics[mNetRx], o.Metrics[mNetTx] = sum(p, vifRx)*8, sum(p, vifTx)*8
			o.Metrics[mDiskRead], o.Metrics[mDiskWrite], o.Metrics[mDiskIOPS] = sum(p, vbdRead), sum(p, vbdWrite), sum(p, vbdIOPS)
		}
		o.Metrics[mSnapshots] = float64(len(v.Snapshots))
		var oldest time.Time
		for _, sref := range v.Snapshots {
			if s, ok := vms[sref]; ok {
				if st, err := parseTime(s.SnapshotTime); err == nil && (oldest.IsZero() || st.Before(oldest)) {
					oldest = st
				}
			}
		}
		if !oldest.IsZero() {
			o.Metrics[mSnapshotAge] = time.Since(oldest).Hours() / 24
		}
		objects = append(objects, o)
	}

	for _, ref := range sortedKeys(srs) {
		s := srs[ref]
		if s.PhysicalSize <= 0 {
			continue // ISO libraries and other SRs without a size
		}
		o := plugin.Object{Key: "sr:" + s.UUID, Name: nameOr(s.NameLabel, s.UUID), Metrics: plugin.NaNs(metricCount),
			Labels: map[string]string{"kind": "sr", "type": s.Type, "shared": fmt.Sprint(s.Shared)}}
		o.Metrics[mSize] = float64(s.PhysicalSize)
		o.Metrics[mUsed] = float64(s.PhysicalUtilisation) / float64(s.PhysicalSize) * 100
		o.Output = fmt.Sprintf("%.1f %% of %s used", o.Metrics[mUsed], bytesText(float64(s.PhysicalSize)))
		objects = append(objects, o)
	}

	out := fmt.Sprintf("pool %s: %d hosts, %d VMs (%d running), HA %s", nameOr(pool.NameLabel, "(unnamed)"),
		len(hosts), total, running, map[bool]string{true: "on", false: "off"}[pool.HAEnabled])
	if len(down) > 0 {
		out += "; not live: " + strings.Join(down, ", ")
	}
	if len(perfErrs) > 0 {
		out += "; performance data missing from " + strings.Join(perfErrs, "; ")
	}
	if objects == nil {
		objects = []plugin.Object{}
	}
	if m, ok := hosts[pool.Master]; ok {
		inv.Device = &plugin.DeviceInfo{Vendor: m.SoftwareVersion["product_brand"], Firmware: m.SoftwareVersion["product_version"]}
	}
	return plugin.Batch{Status: plugin.OK, Output: out, Objects: objects}, inv, nil
}

// failed turns an error into a failed collection: no answer is CRITICAL;
// a rejected login or a refused address is UNKNOWN.
func failed(addr string, err error) plugin.Batch {
	var pe *plugin.PolicyError
	var ae *apiError
	var ne net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &pe):
		return plugin.Batch{Status: plugin.Unknown, Output: pe.Error()}
	case errors.As(err, &ae) && ae.Code == "SESSION_AUTHENTICATION_FAILED":
		return plugin.Batch{Status: plugin.Unknown, Output: "the pool rejected the login: check the xapi credential"}
	case errors.As(err, &ae) && (ae.Code == "RBAC_PERMISSION_DENIED" || ae.Code == "PERMISSION_DENIED"):
		return plugin.Batch{Status: plugin.Unknown, Output: "the account may not read the pool: give it at least the read-only role (" + ae.Error() + ")"}
	case errors.As(err, &ae):
		return plugin.Batch{Status: plugin.Unknown, Output: ae.Error()}
	case errors.As(err, &ne) && ne.Timeout(), errors.As(err, &op), errors.Is(err, context.DeadlineExceeded):
		return plugin.Batch{Status: plugin.Critical, Output: fmt.Sprintf("pool master %s does not answer: %v", addr, err)}
	}
	return plugin.Batch{Status: plugin.Unknown, Output: fmt.Sprintf("pool %s: %v", addr, err)}
}

var (
	pifRx    = regexp.MustCompile(`^pif_eth[0-9]+_rx$`)
	pifTx    = regexp.MustCompile(`^pif_eth[0-9]+_tx$`)
	vmCPU    = regexp.MustCompile(`^cpu[0-9]+$`)
	vifRx    = regexp.MustCompile(`^vif_[0-9]+_rx$`)
	vifTx    = regexp.MustCompile(`^vif_[0-9]+_tx$`)
	vbdRead  = regexp.MustCompile(`^vbd_[a-z0-9]+_read$`)
	vbdWrite = regexp.MustCompile(`^vbd_[a-z0-9]+_write$`)
	vbdIOPS  = regexp.MustCompile(`^vbd_[a-z0-9]+_iops_total$`)
)

func sum(p map[string]float64, re *regexp.Regexp) float64 {
	total, any := 0.0, false
	for k, v := range p {
		if re.MatchString(k) {
			total, any = total+v, true
		}
	}
	if !any {
		return math.NaN()
	}
	return total
}

func mean(p map[string]float64, re *regexp.Regexp) float64 {
	total, n := 0.0, 0
	for k, v := range p {
		if re.MatchString(k) {
			total, n = total+v, n+1
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return total / float64(n)
}

// hostAddress is where a pool member's performance data is read: its
// address, with the port used for the master when the API port is not 443.
func hostAddress(addr, master string) string {
	return withPort(addr, master)
}

// parseTime reads XAPI datetimes such as 20261007T10:20:30Z.
func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{"20060102T15:04:05Z", time.RFC3339, "20060102T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown time %q", s)
}

func levelStatus(s string) plugin.Status {
	switch s {
	case "warning":
		return plugin.Warning
	case "critical":
		return plugin.Critical
	}
	return plugin.OK
}

func nameOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return strings.TrimSpace(s)
}

func bytesText(n float64) string {
	switch {
	case n >= 1e12:
		return fmt.Sprintf("%.1f TB", n/1e12)
	case n >= 1e9:
		return fmt.Sprintf("%.0f GB", n/1e9)
	}
	return fmt.Sprintf("%.0f MB", n/1e6)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
