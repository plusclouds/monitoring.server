package snmp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Interfaces{}) }

// InterfacesConfig of an snmp.interfaces collector.
type InterfacesConfig struct {
	Connection
	Include       string `json:"include,omitempty" jsonschema:"maxLength=1024,description=Regular expression: only interfaces whose name or description matches"`
	Exclude       string `json:"exclude,omitempty" jsonschema:"maxLength=1024,description=Regular expression: skip interfaces whose name or description matches"`
	Types         []int  `json:"types,omitempty" jsonschema:"description=Only these ifType numbers (6 is Ethernet). Default: every type except software loopback (24)"`
	AdminUpOnly   *bool  `json:"admin_up_only,omitempty" jsonschema:"default=true,description=Skip interfaces that are administratively down"`
	DownStatus    string `json:"down_status,omitempty" jsonschema:"enum=critical,enum=warning,enum=ok,default=critical,description=Status of an interface that is administratively up but operationally down"`
	MaxInterfaces int    `json:"max_interfaces,omitempty" jsonschema:"minimum=1,maximum=5000,default=1000"`
}

var interfacesDefaults = InterfacesConfig{DownStatus: "critical", MaxInterfaces: 1000}

// Metric slots of snmp.interfaces, per interface.
const (
	iInBps = iota
	iOutBps
	iInErrors
	iOutErrors
	iInDiscards
	iOutDiscards
	iOperStatus
	iSpeed
	interfaceMetrics
)

// Interfaces implements the snmp.interfaces collector: traffic, errors,
// discards and status of every interface, each an object of the check.
type Interfaces struct{}

func (*Interfaces) Manifest() plugin.Manifest {
	rate := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "rate", RetentionClass: plugin.RetentionStandard}
	}
	gauge := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type:            "snmp.interfaces",
		Kind:            plugin.KindCollector,
		Description:     "SNMP: traffic, errors, discards and status of every interface (IF-MIB, 64-bit counters). Each interface is an object with its own status, thresholds and incidents.",
		ConfigSchema:    plugin.SchemaFor[InterfacesConfig](),
		CredentialTypes: credentialTypes, CredentialsRequired: true,
		Metrics: []plugin.MetricDef{
			rate("in_bps", "bit/s", "Inbound traffic"),
			rate("out_bps", "bit/s", "Outbound traffic"),
			rate("in_errors_rate", "1/s", "Inbound errors"),
			rate("out_errors_rate", "1/s", "Outbound errors"),
			rate("in_discards_rate", "1/s", "Inbound discards"),
			rate("out_discards_rate", "1/s", "Outbound discards"),
			gauge("oper_status", "1", "ifOperStatus: 1 up, 2 down, 3 testing, 5 dormant, 6 not present, 7 lower layer down"),
			gauge("speed_bps", "bit/s", "Interface speed"),
		},
		DefaultInterval: time.Minute,
		MinInterval:     time.Minute,
		Slow:            true,
		BillingClass:    plugin.BillingStandard,
	}
}

func (*Interfaces) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, interfacesDefaults)
	if err != nil {
		return err
	}
	for name, re := range map[string]string{"include": cfg.Include, "exclude": cfg.Exclude} {
		if _, err := regexp.Compile(re); err != nil {
			return fmt.Errorf("%s is not a valid regular expression: %w", name, err)
		}
	}
	return nil
}

// IF-MIB columns, in the order table() returns them.
var ifColumns = []string{
	".1.3.6.1.2.1.2.2.1.2",     // 0 ifDescr
	".1.3.6.1.2.1.2.2.1.3",     // 1 ifType
	".1.3.6.1.2.1.2.2.1.5",     // 2 ifSpeed
	".1.3.6.1.2.1.2.2.1.7",     // 3 ifAdminStatus
	".1.3.6.1.2.1.2.2.1.8",     // 4 ifOperStatus
	".1.3.6.1.2.1.2.2.1.10",    // 5 ifInOctets
	".1.3.6.1.2.1.2.2.1.13",    // 6 ifInDiscards
	".1.3.6.1.2.1.2.2.1.14",    // 7 ifInErrors
	".1.3.6.1.2.1.2.2.1.16",    // 8 ifOutOctets
	".1.3.6.1.2.1.2.2.1.19",    // 9 ifOutDiscards
	".1.3.6.1.2.1.2.2.1.20",    // 10 ifOutErrors
	".1.3.6.1.2.1.31.1.1.1.1",  // 11 ifName
	".1.3.6.1.2.1.31.1.1.1.6",  // 12 ifHCInOctets
	".1.3.6.1.2.1.31.1.1.1.10", // 13 ifHCOutOctets
	".1.3.6.1.2.1.31.1.1.1.15", // 14 ifHighSpeed (Mbit/s)
	".1.3.6.1.2.1.31.1.1.1.18", // 15 ifAlias
}

const (
	cDescr = iota
	cType
	cSpeed
	cAdmin
	cOper
	cInOctets
	cInDiscards
	cInErrors
	cOutOctets
	cOutDiscards
	cOutErrors
	cName
	cHCIn
	cHCOut
	cHighSpeed
	cAlias
)

// ifCounters are one interface's counters, kept between runs:
// in and out octets, in and out errors, in and out discards.
type ifCounters struct {
	Values [6]float64 `json:"v"`
	Bits   int        `json:"b"` // of the octet counters: 64 (HC) or 32
}

type ifState struct {
	At       time.Time             `json:"t"`
	Uptime   float64               `json:"u,omitempty"`
	Counters map[string]ifCounters `json:"c"`
}

func (*Interfaces) Collect(ctx context.Context, t plugin.Target) (plugin.Batch, plugin.Inventory, error) {
	cfg, err := plugin.DecodeConfig(t.Config, interfacesDefaults)
	if err != nil {
		return plugin.Batch{}, plugin.Inventory{}, err
	}
	include, _ := regexp.Compile(cfg.Include)
	exclude, _ := regexp.Compile(cfg.Exclude)
	s, err := open(ctx, t, cfg.Connection)
	if err != nil {
		return plugin.Batch{Status: plugin.Unknown, Output: err.Error()}, plugin.Inventory{}, nil
	}
	defer s.close()

	fail := func(err error) (plugin.Batch, plugin.Inventory, error) {
		r := s.failure(err)
		return plugin.Batch{Status: r.Status, Output: r.Output}, plugin.Inventory{}, nil
	}
	head, err := s.get(oidSysUpTime)
	if err != nil {
		return fail(err)
	}
	rows, err := s.table(ifColumns)
	if err != nil {
		return fail(err)
	}
	now := time.Now()
	uptime, _ := uptimeSeconds(head)

	var prev ifState
	if b, ok := t.State.Get("if"); ok {
		_ = json.Unmarshal(b, &prev)
	}
	next := ifState{At: now, Uptime: uptime, Counters: map[string]ifCounters{}}

	// Order by ifIndex; keys by name, which survives renumbering after a reboot.
	indexes := make([]string, 0, len(rows))
	for idx := range rows {
		indexes = append(indexes, idx)
	}
	slices.SortFunc(indexes, func(a, b string) int {
		ai, _ := strconv.Atoi(a)
		bi, _ := strconv.Atoi(b)
		return ai - bi
	})
	names := map[string]int{}
	for _, idx := range indexes {
		names[ifLabel(rows[idx])]++
	}

	adminUpOnly := cfg.AdminUpOnly == nil || *cfg.AdminUpOnly
	var objects []plugin.Object
	up, down := 0, []string{}
	for _, idx := range indexes {
		row := rows[idx]
		name := ifLabel(row)
		typ, _ := num1(row, cType)
		admin, _ := num1(row, cAdmin)
		oper, _ := num1(row, cOper)
		alias := str1(row, cAlias)
		switch {
		case len(cfg.Types) > 0 && !slices.Contains(cfg.Types, int(typ)):
			continue
		case len(cfg.Types) == 0 && typ == 24: // softwareLoopback
			continue
		case adminUpOnly && admin != 1:
			continue
		case cfg.Include != "" && !include.MatchString(name) && !include.MatchString(str1(row, cDescr)):
			continue
		case cfg.Exclude != "" && (exclude.MatchString(name) || exclude.MatchString(str1(row, cDescr))):
			continue
		}
		if len(objects) == cfg.MaxInterfaces {
			break
		}
		key := name
		if names[name] > 1 || name == "" {
			key = "ifIndex." + idx
		}
		speed := ifSpeed(row)
		o := plugin.Object{Key: key, Name: name, Metrics: plugin.NaNs(interfaceMetrics),
			Labels: map[string]string{"if_index": idx, "if_type": strconv.Itoa(int(typ)), "description": str1(row, cDescr)}}
		if alias != "" {
			o.Labels["alias"] = alias
		}
		if speed > 0 {
			o.Metrics[iSpeed] = speed
			o.Labels["speed"] = humanSpeed(speed)
		}
		o.Metrics[iOperStatus] = oper

		cur, ok := counters(row)
		if ok {
			next.Counters[key] = cur
			if p, had := prev.Counters[key]; had && !prev.At.IsZero() {
				rates(&o, p, cur, prev, next, speed)
			}
		}

		switch {
		case admin != 1:
			o.Output = "administratively down"
		case oper == 1:
			o.Output = "up"
			up++
		case oper == 5:
			o.Output = "dormant"
		case oper == 2 || oper == 6 || oper == 7:
			o.Output = "down (" + operText(oper) + ")"
			o.Status = downStatus(cfg.DownStatus)
			down = append(down, name)
		default:
			o.Output = operText(oper)
			o.Status = plugin.Warning
		}
		if speed > 0 && o.Status == plugin.OK && oper == 1 {
			o.Output += ", " + humanSpeed(speed)
		}
		objects = append(objects, o)
	}
	if b, err := json.Marshal(next); err == nil {
		if err := t.State.Set("if", b); err != nil && t.Log != nil {
			t.Log.Warn("interface counters not kept; rates restart next run", "error", err)
		}
	}
	if objects == nil {
		objects = []plugin.Object{}
	}
	out := fmt.Sprintf("%d interfaces: %d up", len(objects), up)
	if len(down) > 0 {
		shown := down
		if len(shown) > 10 {
			shown = shown[:10]
		}
		out += fmt.Sprintf(", %d down (%s)", len(down), strings.Join(shown, ", "))
	}
	return plugin.Batch{Status: plugin.OK, Output: out, Objects: objects}, plugin.Inventory{}, nil
}

// rates fills the per-second rates from the previous run's counters.
func rates(o *plugin.Object, p, cur ifCounters, prev, next ifState, speed float64) {
	mk := func(i, bits int, st ifState, c ifCounters) sample {
		return sample{Value: c.Values[i], Bits: bits, At: st.At, Uptime: st.Uptime}
	}
	maxBytes := 0.0
	if speed > 0 {
		maxBytes = speed / 8
	}
	for i, slot := range []int{iInBps, iOutBps} {
		if r, ok := rate(mk(i, p.Bits, prev, p), mk(i, cur.Bits, next, cur), maxBytes); ok {
			o.Metrics[slot] = r * 8
		}
	}
	for i, slot := range []int{iInErrors, iOutErrors, iInDiscards, iOutDiscards} {
		if r, ok := rate(mk(i+2, 32, prev, p), mk(i+2, 32, next, cur), 0); ok {
			o.Metrics[slot] = r
		}
	}
}

// counters reads an interface's counters: 64-bit octets when the agent has
// them, 32-bit otherwise.
func counters(row map[int]gosnmp.SnmpPDU) (ifCounters, bool) {
	var c ifCounters
	in, okIn := num1(row, cHCIn)
	out, okOut := num1(row, cHCOut)
	c.Bits = 64
	if !okIn || !okOut {
		in, okIn = num1(row, cInOctets)
		out, okOut = num1(row, cOutOctets)
		c.Bits = 32
	}
	if !okIn || !okOut {
		return c, false
	}
	c.Values[0], c.Values[1] = in, out
	c.Values[2], _ = num1(row, cInErrors)
	c.Values[3], _ = num1(row, cOutErrors)
	c.Values[4], _ = num1(row, cInDiscards)
	c.Values[5], _ = num1(row, cOutDiscards)
	return c, true
}

// ifSpeed is ifHighSpeed (Mbit/s) when set, else ifSpeed (bit/s), which
// tops out at 4.29 Gbit/s.
func ifSpeed(row map[int]gosnmp.SnmpPDU) float64 {
	if hs, ok := num1(row, cHighSpeed); ok && hs > 0 {
		return hs * 1e6
	}
	s, _ := num1(row, cSpeed)
	return s
}

func ifLabel(row map[int]gosnmp.SnmpPDU) string {
	if n := str1(row, cName); n != "" {
		return n
	}
	return str1(row, cDescr)
}

func num1(row map[int]gosnmp.SnmpPDU, col int) (float64, bool) {
	v, ok := row[col]
	if !ok {
		return 0, false
	}
	return number(v)
}

func str1(row map[int]gosnmp.SnmpPDU, col int) string {
	v, ok := row[col]
	if !ok {
		return ""
	}
	return strings.TrimSpace(text(v))
}

func downStatus(s string) plugin.Status {
	switch s {
	case "warning":
		return plugin.Warning
	case "ok":
		return plugin.OK
	}
	return plugin.Critical
}

func operText(v float64) string {
	switch v {
	case 1:
		return "up"
	case 2:
		return "down"
	case 3:
		return "testing"
	case 5:
		return "dormant"
	case 6:
		return "not present"
	case 7:
		return "lower layer down"
	}
	return "unknown"
}

func humanSpeed(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmtNum(bps/1e9) + " Gbit/s"
	case bps >= 1e6:
		return fmtNum(bps/1e6) + " Mbit/s"
	}
	return fmtNum(math.Round(bps/1e3)) + " kbit/s"
}
