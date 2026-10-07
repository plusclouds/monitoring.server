package snmp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&UPS{}) }

// UPSConfig of an snmp.ups check.
type UPSConfig struct {
	Connection
	Profile string `json:"profile,omitempty" jsonschema:"enum=auto,enum=rfc1628,enum=apc,default=auto,description=auto uses APC PowerNet for APC devices and UPS-MIB (RFC 1628) otherwise"`
}

var upsDefaults = UPSConfig{Profile: "auto"}

// Metric slots of snmp.ups.
const (
	uCharge = iota
	uRuntime
	uLoad
	uInput
	uOutput
	uOnBattery
	uReplace
	upsMetrics
)

// UPS implements the snmp.ups check. Status follows the UPS's own state:
// CRITICAL when the battery is low or the load is unpowered, WARNING on
// battery, on bypass or when the battery needs replacing. Thresholds on the
// metrics (runtime, load) are set on the check.
type UPS struct{}

func (*UPS) Manifest() plugin.Manifest {
	g := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type:            "snmp.ups",
		Kind:            plugin.KindCheck,
		Description:     "SNMP: UPS battery, runtime, load and voltages from UPS-MIB (RFC 1628) or APC PowerNet.",
		ConfigSchema:    plugin.SchemaFor[UPSConfig](),
		CredentialTypes: credentialTypes, CredentialsRequired: true,
		Metrics: []plugin.MetricDef{
			g("battery_charge_percent", "percent", "Remaining battery charge"),
			g("runtime_minutes", "min", "Estimated runtime on battery"),
			g("load_percent", "percent", "Output load"),
			g("input_voltage", "V", "Input voltage (line 1)"),
			g("output_voltage", "V", "Output voltage (line 1)"),
			g("on_battery", "1", "1 while the UPS runs on battery"),
			g("battery_replace_needed", "1", "1 when the UPS reports that the battery needs replacing"),
		},
		DefaultInterval: time.Minute,
		MinInterval:     30 * time.Second,
		BillingClass:    plugin.BillingStandard,
	}
}

func (*UPS) Validate(raw json.RawMessage) error {
	_, err := plugin.DecodeConfig(raw, upsDefaults)
	return err
}

// upsState is what a profile read.
type upsState struct {
	metrics  []float64
	problems []string // WARNING
	critical []string // CRITICAL
	found    bool
}

const apcPrefix = ".1.3.6.1.4.1.318"

func (*UPS) Run(ctx context.Context, t plugin.Target) (plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(t.Config, upsDefaults)
	if err != nil {
		return plugin.Result{}, err
	}
	s, err := open(ctx, t, cfg.Connection)
	if err != nil {
		return plugin.Result{Status: plugin.Unknown, Output: err.Error()}, nil
	}
	defer s.close()

	profileName := cfg.Profile
	if profileName == "auto" {
		v, err := s.get(oidSysObjectID)
		if err != nil {
			return s.failure(err), nil
		}
		profileName = "rfc1628"
		if o, ok := v[oidSysObjectID]; ok && strings.HasPrefix(text(o)+".", apcPrefix+".") {
			profileName = "apc"
		}
	}
	read := rfc1628
	if profileName == "apc" {
		read = apc
	}
	st, err := read(s)
	if err != nil {
		return s.failure(err), nil
	}
	// An APC card without PowerNet values may still serve UPS-MIB.
	if !st.found && cfg.Profile == "auto" && profileName == "apc" {
		profileName = "rfc1628"
		if st, err = rfc1628(s); err != nil {
			return s.failure(err), nil
		}
	}
	r := plugin.Result{Metrics: st.metrics, Time: time.Now().UTC()}
	if !st.found {
		r.Status = plugin.Unknown
		r.Output = fmt.Sprintf("%s has no %s UPS objects", s.addr, profileName)
		return r, nil
	}
	m := st.metrics
	summary := fmt.Sprintf("battery %s, runtime %s min, load %s, in %s V, out %s V (%s)",
		pct(m[uCharge], has(m[uCharge])), val(m[uRuntime]), pct(m[uLoad], has(m[uLoad])), val(m[uInput]), val(m[uOutput]), profileName)
	switch {
	case len(st.critical) > 0:
		r.Status = plugin.Critical
		r.Output = strings.Join(append(st.critical, st.problems...), ", ") + "; " + summary
	case len(st.problems) > 0:
		r.Status = plugin.Warning
		r.Output = strings.Join(st.problems, ", ") + "; " + summary
	default:
		r.Output = "on line power; " + summary
	}
	return r, nil
}

func has(f float64) bool { return !math.IsNaN(f) }

func val(f float64) string {
	if !has(f) {
		return "n/a"
	}
	return fmtNum(f)
}

// UPS-MIB (RFC 1628).
const (
	upsBatteryStatus   = ".1.3.6.1.2.1.33.1.2.1.0" // 1 unknown, 2 normal, 3 low, 4 depleted
	upsMinutesLeft     = ".1.3.6.1.2.1.33.1.2.3.0"
	upsChargeLeft      = ".1.3.6.1.2.1.33.1.2.4.0"
	upsInputVoltage1   = ".1.3.6.1.2.1.33.1.3.3.1.3.1"
	upsOutputSource    = ".1.3.6.1.2.1.33.1.4.1.0" // 3 normal, 4 bypass, 5 battery, 2 none, ...
	upsOutputVoltage1  = ".1.3.6.1.2.1.33.1.4.4.1.2.1"
	upsOutputLoad1     = ".1.3.6.1.2.1.33.1.4.4.1.5.1"
	upsAlarmDescr      = ".1.3.6.1.2.1.33.1.6.2.1.2"
	upsAlarmBatteryBad = ".1.3.6.1.2.1.33.1.6.3.1"
)

func rfc1628(s *session) (upsState, error) {
	st := upsState{metrics: plugin.NaNs(upsMetrics)}
	v, err := s.get(upsBatteryStatus, upsMinutesLeft, upsChargeLeft, upsInputVoltage1, upsOutputSource,
		upsOutputVoltage1, upsOutputLoad1)
	if err != nil {
		return st, err
	}
	set := func(slot int, oid string) {
		if f, ok := num(v, oid); ok {
			st.metrics[slot] = f
			st.found = true
		}
	}
	set(uCharge, upsChargeLeft)
	set(uRuntime, upsMinutesLeft)
	set(uLoad, upsOutputLoad1)
	set(uInput, upsInputVoltage1)
	set(uOutput, upsOutputVoltage1)

	if b, ok := num(v, upsBatteryStatus); ok {
		st.found = true
		switch b {
		case 3:
			st.critical = append(st.critical, "battery low")
		case 4:
			st.critical = append(st.critical, "battery depleted")
		}
	}
	if src, ok := num(v, upsOutputSource); ok {
		st.found = true
		st.metrics[uOnBattery] = 0
		switch src {
		case 2:
			st.critical = append(st.critical, "output is off: the load has no power")
		case 4:
			st.problems = append(st.problems, "on bypass")
		case 5:
			st.metrics[uOnBattery] = 1
			st.problems = append(st.problems, "on battery")
		case 6, 7:
			st.problems = append(st.problems, "on booster or reducer")
		}
	}
	if st.found {
		alarms, err := s.walk(upsAlarmDescr)
		if err != nil {
			return st, err
		}
		st.metrics[uReplace] = 0
		for _, a := range alarms {
			if text(a) == upsAlarmBatteryBad {
				st.metrics[uReplace] = 1
				st.problems = append(st.problems, "battery needs replacing")
			}
		}
	}
	return st, nil
}

// APC PowerNet-MIB.
const (
	apcBatteryStatus   = ".1.3.6.1.4.1.318.1.1.1.2.1.1.0" // 2 normal, 3 low, 4 in fault
	apcBatteryCapacity = ".1.3.6.1.4.1.318.1.1.1.2.2.1.0"
	apcRunTime         = ".1.3.6.1.4.1.318.1.1.1.2.2.3.0" // TimeTicks
	apcReplace         = ".1.3.6.1.4.1.318.1.1.1.2.2.4.0" // 1 no, 2 needs replacing
	apcInputVoltage    = ".1.3.6.1.4.1.318.1.1.1.3.2.1.0"
	apcOutputStatus    = ".1.3.6.1.4.1.318.1.1.1.4.1.1.0"
	apcOutputVoltage   = ".1.3.6.1.4.1.318.1.1.1.4.2.1.0"
	apcOutputLoad      = ".1.3.6.1.4.1.318.1.1.1.4.2.3.0"
)

// apcOutput maps upsBasicOutputStatus values that need attention.
var apcOutput = map[float64]struct {
	critical bool
	text     string
}{
	1: {false, "output status unknown"}, 3: {false, "on battery"}, 5: {false, "timed sleeping"},
	6: {false, "on software bypass"}, 7: {true, "output is off: the load has no power"}, 8: {false, "rebooting"},
	9: {false, "on switched bypass"}, 10: {true, "on bypass after a hardware failure"}, 11: {false, "sleeping until power returns"},
	15: {false, "on battery (eco mode)"}, 16: {false, "in eco mode"},
}

func apc(s *session) (upsState, error) {
	st := upsState{metrics: plugin.NaNs(upsMetrics)}
	v, err := s.get(apcBatteryStatus, apcBatteryCapacity, apcRunTime, apcReplace, apcInputVoltage,
		apcOutputStatus, apcOutputVoltage, apcOutputLoad)
	if err != nil {
		return st, err
	}
	set := func(slot int, oid string, scale float64) {
		if f, ok := num(v, oid); ok {
			st.metrics[slot] = f * scale
			st.found = true
		}
	}
	set(uCharge, apcBatteryCapacity, 1)
	set(uRuntime, apcRunTime, 1.0/100/60) // TimeTicks to minutes
	set(uLoad, apcOutputLoad, 1)
	set(uInput, apcInputVoltage, 1)
	set(uOutput, apcOutputVoltage, 1)
	if b, ok := num(v, apcBatteryStatus); ok {
		st.found = true
		switch b {
		case 3:
			st.critical = append(st.critical, "battery low")
		case 4:
			st.critical = append(st.critical, "battery in fault condition")
		}
	}
	if rp, ok := num(v, apcReplace); ok {
		st.metrics[uReplace] = 0
		if rp == 2 {
			st.metrics[uReplace] = 1
			st.problems = append(st.problems, "battery needs replacing")
		}
	}
	if o, ok := num(v, apcOutputStatus); ok {
		st.found = true
		st.metrics[uOnBattery] = 0
		if o == 3 || o == 15 {
			st.metrics[uOnBattery] = 1
		}
		if a, bad := apcOutput[o]; bad {
			if a.critical {
				st.critical = append(st.critical, a.text)
			} else {
				st.problems = append(st.problems, a.text)
			}
		}
	}
	return st, nil
}
