package snmp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// APC facility devices (PowerNet-MIB): rack PDUs and environmental sensors.
// OIDs and value meanings were checked against PowerNet-MIB (2025).

func init() {
	plugin.Register(&PDU{})
	plugin.Register(&Sensor{})
}

// columns are the OIDs of a table read with table(); rows hold values by
// position in the list.
type columns []string

// rPDU2 (AP8xxx and later) tables; the module column numbers daisy-chained PDUs.
var (
	rpdu2Ident = columns{
		".1.3.6.1.4.1.318.1.1.26.2.1.2", // 0 module
		".1.3.6.1.4.1.318.1.1.26.2.1.3", // 1 name
		".1.3.6.1.4.1.318.1.1.26.2.1.8", // 2 model
		".1.3.6.1.4.1.318.1.1.26.2.1.9", // 3 serial
	}
	rpdu2Device = columns{
		".1.3.6.1.4.1.318.1.1.26.4.3.1.2", // 0 module
		".1.3.6.1.4.1.318.1.1.26.4.3.1.3", // 1 name
		".1.3.6.1.4.1.318.1.1.26.4.3.1.4", // 2 load state: 1 low, 2 normal, 3 near overload, 4 overload, 5 not supported
		".1.3.6.1.4.1.318.1.1.26.4.3.1.5", // 3 power, hundredths of kW
		".1.3.6.1.4.1.318.1.1.26.4.3.1.9", // 4 energy, tenths of kWh
	}
	rpdu2Phase = columns{
		".1.3.6.1.4.1.318.1.1.26.6.3.1.2", // 0 module
		".1.3.6.1.4.1.318.1.1.26.6.3.1.3", // 1 number
		".1.3.6.1.4.1.318.1.1.26.6.3.1.4", // 2 load state
		".1.3.6.1.4.1.318.1.1.26.6.3.1.5", // 3 current, tenths of A
		".1.3.6.1.4.1.318.1.1.26.6.3.1.6", // 4 voltage, V
		".1.3.6.1.4.1.318.1.1.26.6.3.1.7", // 5 power, hundredths of kW
	}
	rpdu2Bank = columns{
		".1.3.6.1.4.1.318.1.1.26.8.3.1.2", // 0 module
		".1.3.6.1.4.1.318.1.1.26.8.3.1.3", // 1 number
		".1.3.6.1.4.1.318.1.1.26.8.3.1.4", // 2 load state
		".1.3.6.1.4.1.318.1.1.26.8.3.1.5", // 3 current, tenths of A
	}
	rpdu2Switched = columns{
		".1.3.6.1.4.1.318.1.1.26.9.2.3.1.2", // 0 module
		".1.3.6.1.4.1.318.1.1.26.9.2.3.1.3", // 1 name
		".1.3.6.1.4.1.318.1.1.26.9.2.3.1.4", // 2 number
		".1.3.6.1.4.1.318.1.1.26.9.2.3.1.5", // 3 state: 1 off, 2 on
	}
	rpdu2Metered = columns{
		".1.3.6.1.4.1.318.1.1.26.9.4.3.1.2", // 0 module
		".1.3.6.1.4.1.318.1.1.26.9.4.3.1.3", // 1 name
		".1.3.6.1.4.1.318.1.1.26.9.4.3.1.4", // 2 number
		".1.3.6.1.4.1.318.1.1.26.9.4.3.1.5", // 3 load state: 1 low, 2 normal, 3 near overload, 4 overload
		".1.3.6.1.4.1.318.1.1.26.9.4.3.1.6", // 4 current, tenths of A
		".1.3.6.1.4.1.318.1.1.26.9.4.3.1.7", // 5 power, W
	}
	// rPDU (AP78xx and AP79xx): phase and bank loads, outlet states, total power.
	rpduLoad = columns{
		".1.3.6.1.4.1.318.1.1.12.2.3.1.1.2", // 0 load, tenths of A
		".1.3.6.1.4.1.318.1.1.12.2.3.1.1.3", // 1 state: 1 normal, 2 low, 3 near overload, 4 overload
		".1.3.6.1.4.1.318.1.1.12.2.3.1.1.4", // 2 phase
		".1.3.6.1.4.1.318.1.1.12.2.3.1.1.5", // 3 bank, 0 for a phase row
	}
	rpduOutlet = columns{
		".1.3.6.1.4.1.318.1.1.12.3.5.1.1.2", // 0 name
		".1.3.6.1.4.1.318.1.1.12.3.5.1.1.4", // 1 state: 1 on, 2 off
	}
	rpduPowerWatts = ".1.3.6.1.4.1.318.1.1.12.1.16.0"
	rpduModel      = ".1.3.6.1.4.1.318.1.1.12.1.5.0"
)

// PDUConfig of an snmp.pdu collector.
type PDUConfig struct {
	Connection
	Profile         string `json:"profile,omitempty" jsonschema:"enum=auto,enum=apc,default=auto,description=Vendor MIB. auto picks it from sysObjectID; APC (rPDU2 and rPDU) is the first supported"`
	Outlets         *bool  `json:"outlets,omitempty" jsonschema:"default=true,description=Report every outlet as an object"`
	OutletOffStatus string `json:"outlet_off_status,omitempty" jsonschema:"enum=ok,enum=warning,enum=critical,default=ok,description=Status of a switched outlet that is off"`
}

var pduDefaults = PDUConfig{Profile: "auto", OutletOffStatus: "ok"}

// Metric slots of snmp.pdu, per object (PDU, phase, bank, outlet).
const (
	pCurrent = iota
	pVoltage
	pPower
	pEnergy
	pOutletOn
	pduMetrics
)

// PDU implements snmp.pdu: the load of a rack PDU, its phases, banks and
// outlets, each an object with the PDU's own load state.
type PDU struct{}

func (*PDU) Manifest() plugin.Manifest {
	g := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type:            "snmp.pdu",
		Kind:            plugin.KindCollector,
		Description:     "SNMP: rack PDU load (APC rPDU2 and rPDU): power and energy of the PDU, current per phase and bank, state, current and power per outlet. Each is an object; the PDU's near-overload and overload states are WARNING and CRITICAL.",
		ConfigSchema:    plugin.SchemaFor[PDUConfig](),
		CredentialTypes: credentialTypes,
		Metrics: []plugin.MetricDef{
			g("current_amps", "A", "Current"),
			g("voltage_volts", "V", "Voltage (phases)"),
			g("power_watts", "W", "Power"),
			g("energy_kwh", "kWh", "Energy since the PDU's meter was last reset"),
			g("outlet_on", "1", "1 when a switched outlet is on, 0 when it is off"),
		},
		DefaultInterval: time.Minute,
		MinInterval:     30 * time.Second,
		Slow:            true,
		BillingClass:    plugin.BillingStandard,
	}
}

func (*PDU) Validate(raw json.RawMessage) error {
	_, err := plugin.DecodeConfig(raw, pduDefaults)
	return err
}

func (*PDU) Collect(ctx context.Context, t plugin.Target) (plugin.Batch, plugin.Inventory, error) {
	cfg, err := plugin.DecodeConfig(t.Config, pduDefaults)
	if err != nil {
		return plugin.Batch{}, plugin.Inventory{}, err
	}
	s, err := open(ctx, t, cfg.Connection)
	if err != nil {
		return plugin.Batch{Status: plugin.Unknown, Output: err.Error()}, plugin.Inventory{}, nil
	}
	defer s.close()
	fail := func(err error) (plugin.Batch, plugin.Inventory, error) {
		r := s.failure(err)
		return plugin.Batch{Status: r.Status, Output: r.Output}, plugin.Inventory{}, nil
	}
	head, err := s.get(oidSysObjectID)
	if err != nil {
		return fail(err)
	}
	if cfg.Profile == "auto" && !isAPC(head) {
		return plugin.Batch{Status: plugin.Unknown, Output: s.addr + " is not an APC PDU; other PDU vendors are not supported yet"}, plugin.Inventory{}, nil
	}
	p := &pduRun{s: s, cfg: cfg, outlets: cfg.Outlets == nil || *cfg.Outlets}
	if err := p.rpdu2(); err != nil {
		return fail(err)
	}
	if len(p.objects) == 0 { // older rPDU models
		if err := p.rpdu(); err != nil {
			return fail(err)
		}
	}
	if len(p.objects) == 0 {
		return plugin.Batch{Status: plugin.Unknown, Output: s.addr + " answers, but has no APC PDU objects"}, plugin.Inventory{}, nil
	}
	return plugin.Batch{Status: plugin.OK, Output: p.summary(), Objects: p.objects}, plugin.Inventory{}, nil
}

func isAPC(vals map[string]gosnmp.SnmpPDU) bool {
	o, ok := vals[oidSysObjectID]
	return ok && strings.HasPrefix(text(o)+".", apcPrefix+".")
}

type pduRun struct {
	s       *session
	cfg     PDUConfig
	outlets bool
	objects []plugin.Object
	model   string
}

// scope prefixes a key with the PDU module, except for the first PDU, so
// keys stay the same when a second PDU is daisy-chained later.
func scope(module string) string {
	if module == "" || module == "1" {
		return ""
	}
	return module + "."
}

func (p *pduRun) add(key, name string, st plugin.Status, out string, labels map[string]string, m []float64) {
	o := plugin.Object{Key: key, Name: name, Status: st, Output: out, Metrics: m, Labels: map[string]string{}}
	for k, v := range labels {
		if v != "" {
			o.Labels[k] = v
		}
	}
	p.objects = append(p.objects, o)
}

func (p *pduRun) rpdu2() error {
	ident, err := p.s.table(rpdu2Ident)
	if err != nil {
		return err
	}
	models := map[string]map[int]gosnmp.SnmpPDU{}
	for _, r := range ident {
		models[str1(r, 0)] = r
	}
	dev, err := p.s.table(rpdu2Device)
	if err != nil {
		return err
	}
	for _, idx := range sortedKeys(dev) {
		r := dev[idx]
		module := str1(r, 0)
		id := models[module]
		if p.model == "" {
			p.model = str1(id, 2)
		}
		m := plugin.NaNs(pduMetrics)
		m[pPower] = scaled(r, 3, 10)   // hundredths of kW to W
		m[pEnergy] = scaled(r, 4, 0.1) // tenths of kWh
		st, out := loadState(r, 2, map[float64]int{1: 0, 2: 0, 3: 1, 4: 2})
		p.add("pdu:"+nameOr1(module, "1"), nameOr1(str1(r, 1), "PDU "+module), st, join(out, watts(m[pPower])),
			map[string]string{"kind": "pdu", "model": str1(id, 2), "serial": str1(id, 3)}, m)
	}
	phases, err := p.s.table(rpdu2Phase)
	if err != nil {
		return err
	}
	for _, idx := range sortedKeys(phases) {
		r := phases[idx]
		n := str1(r, 1)
		m := plugin.NaNs(pduMetrics)
		m[pCurrent], m[pVoltage], m[pPower] = scaled(r, 3, 0.1), scaled(r, 4, 1), scaled(r, 5, 10)
		st, out := loadState(r, 2, map[float64]int{1: 0, 2: 0, 3: 1, 4: 2})
		p.add("phase:"+scope(str1(r, 0))+n, "Phase "+n, st, join(out, amps(m[pCurrent])), map[string]string{"kind": "phase"}, m)
	}
	banks, err := p.s.table(rpdu2Bank)
	if err != nil {
		return err
	}
	for _, idx := range sortedKeys(banks) {
		r := banks[idx]
		n := str1(r, 1)
		m := plugin.NaNs(pduMetrics)
		m[pCurrent] = scaled(r, 3, 0.1)
		st, out := loadState(r, 2, map[float64]int{1: 0, 2: 0, 3: 1, 4: 2})
		p.add("bank:"+scope(str1(r, 0))+n, "Bank "+n, st, join(out, amps(m[pCurrent])), map[string]string{"kind": "bank"}, m)
	}
	if !p.outlets {
		return nil
	}
	// An outlet can be switched, metered, or both: merge by module and number.
	type outlet struct {
		module, number, name string
		state, load          float64
		current, power       float64
		switched, metered    bool
	}
	byKey := map[string]*outlet{}
	var order []string
	get := func(module, number string) *outlet {
		k := scope(module) + number
		if o, ok := byKey[k]; ok {
			return o
		}
		o := &outlet{module: module, number: number, current: math.NaN(), power: math.NaN()}
		byKey[k] = o
		order = append(order, k)
		return o
	}
	sw, err := p.s.table(rpdu2Switched)
	if err != nil {
		return err
	}
	for _, idx := range sortedKeys(sw) {
		r := sw[idx]
		o := get(str1(r, 0), str1(r, 2))
		o.name, o.switched = str1(r, 1), true
		o.state, _ = num1(r, 3)
	}
	met, err := p.s.table(rpdu2Metered)
	if err != nil {
		return err
	}
	for _, idx := range sortedKeys(met) {
		r := met[idx]
		o := get(str1(r, 0), str1(r, 2))
		if o.name == "" {
			o.name = str1(r, 1)
		}
		o.metered = true
		o.load, _ = num1(r, 3)
		o.current, o.power = scaled(r, 4, 0.1), scaled(r, 5, 1)
	}
	for _, k := range order {
		o := byKey[k]
		m := plugin.NaNs(pduMetrics)
		m[pCurrent], m[pPower] = o.current, o.power
		st, out := plugin.OK, ""
		if o.metered {
			switch o.load {
			case 3:
				st, out = plugin.Warning, "near overload"
			case 4:
				st, out = plugin.Critical, "overload"
			}
		}
		if o.switched {
			if o.state == 2 {
				m[pOutletOn] = 1
				out = join(out, "on")
			} else {
				m[pOutletOn] = 0
				out = join(out, "off")
				st = worse(st, levelStatus(p.cfg.OutletOffStatus))
			}
		}
		p.add("outlet:"+k, nameOr1(o.name, "Outlet "+o.number), st, join(out, amps(o.current)),
			map[string]string{"kind": "outlet", "number": o.number}, m)
	}
	return nil
}

func (p *pduRun) rpdu() error {
	head, err := p.s.get(rpduPowerWatts, rpduModel)
	if err != nil {
		return err
	}
	load, err := p.s.table(rpduLoad)
	if err != nil {
		return err
	}
	if len(load) == 0 {
		return nil
	}
	if v, ok := head[rpduModel]; ok {
		p.model = text(v)
	}
	m := plugin.NaNs(pduMetrics)
	if w, ok := num(head, rpduPowerWatts); ok && w >= 0 {
		m[pPower] = w
	}
	p.add("pdu:1", "PDU", plugin.OK, watts(m[pPower]), map[string]string{"kind": "pdu", "model": p.model}, m)
	for _, idx := range sortedKeys(load) {
		r := load[idx]
		m := plugin.NaNs(pduMetrics)
		m[pCurrent] = scaled(r, 0, 0.1)
		st, out := loadState(r, 1, map[float64]int{1: 0, 2: 0, 3: 1, 4: 2})
		phase, _ := num1(r, 2)
		bank, _ := num1(r, 3)
		if bank > 0 {
			n := strconv.Itoa(int(bank))
			p.add("bank:"+n, "Bank "+n, st, join(out, amps(m[pCurrent])), map[string]string{"kind": "bank"}, m)
		} else {
			n := strconv.Itoa(int(phase))
			p.add("phase:"+n, "Phase "+n, st, join(out, amps(m[pCurrent])), map[string]string{"kind": "phase"}, m)
		}
	}
	if !p.outlets {
		return nil
	}
	outs, err := p.s.table(rpduOutlet)
	if err != nil {
		return err
	}
	for _, idx := range sortedKeys(outs) {
		r := outs[idx]
		m := plugin.NaNs(pduMetrics)
		state, _ := num1(r, 1)
		st, out := plugin.OK, "on"
		m[pOutletOn] = 1
		if state == 2 {
			st, out, m[pOutletOn] = levelStatus(p.cfg.OutletOffStatus), "off", 0
		}
		p.add("outlet:"+idx, nameOr1(str1(r, 0), "Outlet "+idx), st, out, map[string]string{"kind": "outlet", "number": idx}, m)
	}
	return nil
}

func (p *pduRun) summary() string {
	var bad []string
	power := math.NaN()
	for _, o := range p.objects {
		if o.Status != plugin.OK {
			bad = append(bad, o.Name+" "+o.Output)
		}
		if o.Labels["kind"] == "pdu" && !math.IsNaN(o.Metrics[pPower]) {
			if math.IsNaN(power) {
				power = 0
			}
			power += o.Metrics[pPower]
		}
	}
	out := "APC PDU"
	if p.model != "" {
		out += " " + p.model
	}
	if !math.IsNaN(power) {
		out += fmt.Sprintf(", %s W", fmtNum(power))
	}
	if len(bad) == 0 {
		return out + ", all normal"
	}
	if len(bad) > 5 {
		bad = append(bad[:5], "...")
	}
	return out + ": " + strings.Join(bad, "; ")
}

// Sensors: probes on a rack PDU (rPDU2) and on network management cards
// (universal I/O, AP9631 and NMC3 with AP9335T/TH probes and dry contacts).
var (
	rpdu2TempHumidity = columns{
		".1.3.6.1.4.1.318.1.1.26.10.2.2.1.2",  // 0 module
		".1.3.6.1.4.1.318.1.1.26.10.2.2.1.3",  // 1 name
		".1.3.6.1.4.1.318.1.1.26.10.2.2.1.6",  // 2 comm: 1 not installed, 2 OK, 3 lost
		".1.3.6.1.4.1.318.1.1.26.10.2.2.1.8",  // 3 temperature, tenths of °C
		".1.3.6.1.4.1.318.1.1.26.10.2.2.1.9",  // 4 temp status: 1 not present, 2 below min, 3 below low, 4 normal, 5 above high, 6 above max
		".1.3.6.1.4.1.318.1.1.26.10.2.2.1.10", // 5 humidity, %
		".1.3.6.1.4.1.318.1.1.26.10.2.2.1.11", // 6 humidity status, as temp status
	}
	rpdu2Discrete = columns{
		".1.3.6.1.4.1.318.1.1.26.10.4.2.1.2", // 0 module
		".1.3.6.1.4.1.318.1.1.26.10.4.2.1.3", // 1 name
		".1.3.6.1.4.1.318.1.1.26.10.4.2.1.5", // 2 type: 1 not connected, 2 door, 3 smoke, 4 motion, 5 vibration, 6 dry contact, 7 spot leak
		".1.3.6.1.4.1.318.1.1.26.10.4.2.1.7", // 3 state: 1 open, 2 closed, 3 unknown
		".1.3.6.1.4.1.318.1.1.26.10.4.2.1.8", // 4 alarm: 1 normal, 2 alarm
	}
	uioSensor = columns{
		".1.3.6.1.4.1.318.1.1.25.1.2.1.3",  // 0 name
		".1.3.6.1.4.1.318.1.1.25.1.2.1.6",  // 1 temperature, °C; -1 invalid
		".1.3.6.1.4.1.318.1.1.25.1.2.1.7",  // 2 humidity, %; -1 none
		".1.3.6.1.4.1.318.1.1.25.1.2.1.9",  // 3 alarm: 1 normal, 2 warning, 3 critical, 4 n/a
		".1.3.6.1.4.1.318.1.1.25.1.2.1.10", // 4 comm: 1 not installed, 2 OK, 3 lost
	}
	uioContact = columns{
		".1.3.6.1.4.1.318.1.1.25.2.2.1.3", // 0 name
		".1.3.6.1.4.1.318.1.1.25.2.2.1.5", // 1 state: 1 closed, 2 open, 3 disabled, 4 n/a
		".1.3.6.1.4.1.318.1.1.25.2.2.1.6", // 2 alarm: 1 normal, 2 warning, 3 critical, 4 n/a
		".1.3.6.1.4.1.318.1.1.25.2.2.1.7", // 3 comm
	}
)

var discreteTypes = map[float64]string{2: "door", 3: "smoke", 4: "motion", 5: "vibration", 6: "dry contact", 7: "leak"}

// SensorConfig of an snmp.sensor collector.
type SensorConfig struct {
	Connection
	Profile string `json:"profile,omitempty" jsonschema:"enum=auto,enum=apc,default=auto,description=Vendor MIB. APC (rack PDU probes and network management card probes) is the first supported"`
}

var sensorDefaults = SensorConfig{Profile: "auto"}

// Metric slots of snmp.sensor, per sensor.
const (
	sTemperature = iota
	sHumidity
	sContactOpen
	sensorMetrics
)

// Sensor implements snmp.sensor: temperature, humidity, door, leak and
// other contact sensors, each an object with the device's own alarm state.
type Sensor struct{}

func (*Sensor) Manifest() plugin.Manifest {
	g := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type:            "snmp.sensor",
		Kind:            plugin.KindCollector,
		Description:     "SNMP: environmental sensors (APC probes on rack PDUs and network management cards): temperature, humidity, door, leak, smoke and other contacts. Status follows the device's own alarm thresholds.",
		ConfigSchema:    plugin.SchemaFor[SensorConfig](),
		CredentialTypes: credentialTypes,
		Metrics: []plugin.MetricDef{
			g("temperature_celsius", "Cel", "Temperature"),
			g("humidity_percent", "percent", "Relative humidity"),
			g("contact_open", "1", "1 when a contact (door, leak, smoke) is open, 0 when closed"),
		},
		DefaultInterval: time.Minute,
		MinInterval:     30 * time.Second,
		Slow:            true,
		BillingClass:    plugin.BillingStandard,
	}
}

func (*Sensor) Validate(raw json.RawMessage) error {
	_, err := plugin.DecodeConfig(raw, sensorDefaults)
	return err
}

func (*Sensor) Collect(ctx context.Context, t plugin.Target) (plugin.Batch, plugin.Inventory, error) {
	cfg, err := plugin.DecodeConfig(t.Config, sensorDefaults)
	if err != nil {
		return plugin.Batch{}, plugin.Inventory{}, err
	}
	s, err := open(ctx, t, cfg.Connection)
	if err != nil {
		return plugin.Batch{Status: plugin.Unknown, Output: err.Error()}, plugin.Inventory{}, nil
	}
	defer s.close()
	fail := func(err error) (plugin.Batch, plugin.Inventory, error) {
		r := s.failure(err)
		return plugin.Batch{Status: r.Status, Output: r.Output}, plugin.Inventory{}, nil
	}
	head, err := s.get(oidSysObjectID)
	if err != nil {
		return fail(err)
	}
	if cfg.Profile == "auto" && !isAPC(head) {
		return plugin.Batch{Status: plugin.Unknown, Output: s.addr + " is not an APC device; other sensor vendors are not supported yet"}, plugin.Inventory{}, nil
	}
	var objs []plugin.Object
	add := func(key, name, kind string, st plugin.Status, out string, m []float64) {
		objs = append(objs, plugin.Object{Key: key, Name: name, Status: st, Output: out, Metrics: m,
			Labels: map[string]string{"kind": kind}})
	}

	th, err := s.table(rpdu2TempHumidity)
	if err != nil {
		return fail(err)
	}
	for _, idx := range sortedKeys(th) {
		r := th[idx]
		comm, _ := num1(r, 2)
		if comm == 1 {
			continue
		}
		m := plugin.NaNs(sensorMetrics)
		st, out := plugin.OK, ""
		if comm == 3 {
			st, out = plugin.Unknown, "communication with the probe lost"
		} else {
			if ts, _ := num1(r, 4); ts != 1 {
				m[sTemperature] = scaled(r, 3, 0.1)
				st, out = worse(st, probeStatus(ts)), join(out, probeText(ts, "temperature"))
			}
			if hs, ok := num1(r, 6); ok && hs != 1 {
				m[sHumidity] = scaled(r, 5, 1)
				st, out = worse(st, probeStatus(hs)), join(out, probeText(hs, "humidity"))
			}
			out = join(out, reading(m))
		}
		add("probe:"+scope(str1(r, 0))+idx, nameOr1(str1(r, 1), "Probe "+idx), "probe", st, out, m)
	}
	disc, err := s.table(rpdu2Discrete)
	if err != nil {
		return fail(err)
	}
	for _, idx := range sortedKeys(disc) {
		r := disc[idx]
		typ, _ := num1(r, 2)
		kind, ok := discreteTypes[typ]
		if !ok {
			continue // not connected
		}
		m := plugin.NaNs(sensorMetrics)
		state, _ := num1(r, 3)
		alarm, _ := num1(r, 4)
		out := map[float64]string{1: "open", 2: "closed"}[state]
		if state == 1 || state == 2 {
			m[sContactOpen] = map[float64]float64{1: 1, 2: 0}[state]
		}
		st := plugin.OK
		if alarm == 2 {
			st, out = plugin.Critical, join("alarm", out)
		}
		add("contact:"+scope(str1(r, 0))+idx, nameOr1(str1(r, 1), kind+" "+idx), kind, st, nameOr1(out, "state unknown"), m)
	}

	uio, err := s.table(uioSensor)
	if err != nil {
		return fail(err)
	}
	for _, idx := range sortedKeys(uio) {
		r := uio[idx]
		comm, _ := num1(r, 4)
		if comm == 1 {
			continue
		}
		m := plugin.NaNs(sensorMetrics)
		if v, ok := num1(r, 1); ok && v != -1 {
			m[sTemperature] = v
		}
		if v, ok := num1(r, 2); ok && v != -1 {
			m[sHumidity] = v
		}
		alarm, _ := num1(r, 3)
		st, out := alarmStatus(alarm), reading(m)
		if comm == 3 {
			st, out = plugin.Unknown, "communication with the probe lost"
		}
		add("probe:uio."+idx, nameOr1(str1(r, 0), "Probe "+idx), "probe", st, out, m)
	}
	contacts, err := s.table(uioContact)
	if err != nil {
		return fail(err)
	}
	for _, idx := range sortedKeys(contacts) {
		r := contacts[idx]
		state, _ := num1(r, 1)
		if state == 3 || state == 4 {
			continue // disabled or not applicable
		}
		m := plugin.NaNs(sensorMetrics)
		m[sContactOpen] = map[float64]float64{1: 0, 2: 1}[state]
		alarm, _ := num1(r, 2)
		out := map[float64]string{1: "closed", 2: "open"}[state]
		st := alarmStatus(alarm)
		if st != plugin.OK {
			out = join("alarm", out)
		}
		add("contact:uio."+idx, nameOr1(str1(r, 0), "Contact "+idx), "contact", st, out, m)
	}

	if len(objs) == 0 {
		return plugin.Batch{Status: plugin.Unknown, Output: s.addr + " answers, but reports no sensors"}, plugin.Inventory{}, nil
	}
	var bad []string
	for _, o := range objs {
		if o.Status != plugin.OK {
			bad = append(bad, o.Name+": "+o.Output)
		}
	}
	out := fmt.Sprintf("%d sensors", len(objs))
	if len(bad) == 0 {
		out += ", all normal"
	} else {
		out += ", " + strings.Join(bad, "; ")
	}
	return plugin.Batch{Status: plugin.OK, Output: out, Objects: objs}, plugin.Inventory{}, nil
}

// probeStatus maps the rPDU2 probe state: below min and above max are
// CRITICAL, below low and above high WARNING.
func probeStatus(v float64) plugin.Status {
	switch v {
	case 2, 6:
		return plugin.Critical
	case 3, 5:
		return plugin.Warning
	}
	return plugin.OK
}

func probeText(v float64, what string) string {
	return map[float64]string{2: what + " below minimum", 3: what + " low", 5: what + " high", 6: what + " above maximum"}[v]
}

// alarmStatus maps the universal I/O alarm state.
func alarmStatus(v float64) plugin.Status {
	switch v {
	case 2:
		return plugin.Warning
	case 3:
		return plugin.Critical
	}
	return plugin.OK
}

func reading(m []float64) string {
	var parts []string
	if v := m[sTemperature]; !math.IsNaN(v) {
		parts = append(parts, fmtNum(v)+" °C")
	}
	if v := m[sHumidity]; !math.IsNaN(v) {
		parts = append(parts, fmtNum(v)+" %RH")
	}
	return strings.Join(parts, ", ")
}

// loadState maps an APC load state column to a status and a text; levels
// gives the level (0 OK, 1 warning, 2 critical) per value.
func loadState(r map[int]gosnmp.SnmpPDU, col int, levels map[float64]int) (plugin.Status, string) {
	v, ok := num1(r, col)
	if !ok {
		return plugin.OK, ""
	}
	switch levels[v] {
	case 1:
		return plugin.Warning, "near overload"
	case 2:
		return plugin.Critical, "overload"
	}
	return plugin.OK, ""
}

// scaled reads a column times f; APC reports -1 for unsupported values.
func scaled(r map[int]gosnmp.SnmpPDU, col int, f float64) float64 {
	v, ok := num1(r, col)
	if !ok || v < 0 {
		return math.NaN()
	}
	return math.Round(v*f*1e6) / 1e6 // 82 tenths are 8.2 A, not 8.200000000000001
}

func amps(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return fmtNum(v) + " A"
}

func watts(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return fmtNum(v) + " W"
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

func worse(a, b plugin.Status) plugin.Status {
	rank := map[plugin.Status]int{plugin.OK: 0, plugin.Unknown: 1, plugin.Warning: 2, plugin.Critical: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func join(a, b string) string {
	a, b = strings.TrimSuffix(strings.TrimSpace(a), ","), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + ", " + b
}

func nameOr1(s string, def ...string) string {
	if strings.TrimSpace(s) != "" || len(def) == 0 {
		return strings.TrimSpace(s)
	}
	return def[0]
}

func sortedKeys(rows map[string]map[int]gosnmp.SnmpPDU) []string {
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sortIndexes(keys)
	return keys
}

// sortIndexes orders OID indexes numerically, arc by arc.
func sortIndexes(keys []string) {
	less := func(a, b string) bool { return oidAfter(b, a) }
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && less(keys[j], keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}
