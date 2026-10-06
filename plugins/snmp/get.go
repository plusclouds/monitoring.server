package snmp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Get{}) }

// GetConfig of an snmp.get check.
type GetConfig struct {
	Connection
	OID    string  `json:"oid" jsonschema:"required,description=Numeric OID to read, e.g. 1.3.6.1.4.1.2021.10.1.3.1"`
	Kind   string  `json:"kind,omitempty" jsonschema:"enum=gauge,enum=counter,default=gauge,description=gauge stores the value; counter stores the per-second rate"`
	Scale  float64 `json:"scale,omitempty" jsonschema:"description=Multiplier applied to the value (0.1 for tenths). Default: 1"`
	Expect string  `json:"expect,omitempty" jsonschema:"maxLength=1024,description=CRITICAL unless the value matches this"`
	Match  string  `json:"match,omitempty" jsonschema:"enum=equals,enum=contains,enum=regex,default=equals,description=How expect is compared with the value as text"`
}

var getDefaults = GetConfig{Kind: "gauge", Match: "equals"}

var oidRe = regexp.MustCompile(`^\.?[0-9]+(\.[0-9]+)+$`)

// Metric slots of snmp.get.
const (
	gValue = iota
	gRate
)

// Get implements the snmp.get check: one OID as a gauge or a counter rate.
// It is the escape hatch for devices without a profile; several OIDs are
// several checks.
type Get struct{}

func (*Get) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type:            "snmp.get",
		Kind:            plugin.KindCheck,
		Description:     "SNMP: read one OID as a gauge or a counter rate, optionally compared with an expected value.",
		ConfigSchema:    plugin.SchemaFor[GetConfig](),
		CredentialTypes: credentialTypes,
		Metrics: []plugin.MetricDef{
			{Name: "value", Unit: "1", Description: "Value of the OID times scale (kind gauge)", Kind: "gauge", RetentionClass: plugin.RetentionStandard},
			{Name: "rate", Unit: "1/s", Description: "Per-second rate of the counter times scale (kind counter)", Kind: "rate", RetentionClass: plugin.RetentionStandard},
		},
		DefaultInterval: time.Minute,
		MinInterval:     10 * time.Second,
		BillingClass:    plugin.BillingStandard,
		WhoopsyMetric:   "value",
	}
}

func (*Get) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, getDefaults)
	if err != nil {
		return err
	}
	if !oidRe.MatchString(cfg.OID) {
		return fmt.Errorf("oid %q is not a numeric OID", cfg.OID)
	}
	if cfg.Match == "regex" {
		if _, err := regexp.Compile(cfg.Expect); err != nil {
			return fmt.Errorf("expect is not a valid regular expression: %w", err)
		}
	}
	return nil
}

func (g *Get) Run(ctx context.Context, t plugin.Target) (plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(t.Config, getDefaults)
	if err != nil {
		return plugin.Result{}, err
	}
	if cfg.Scale == 0 {
		cfg.Scale = 1
	}
	s, err := open(ctx, t, cfg.Connection)
	if err != nil {
		return plugin.Result{Status: plugin.Unknown, Output: err.Error()}, nil
	}
	defer s.close()

	oid := normalizeOID(cfg.OID)
	vals, err := s.get(oid, oidSysUpTime)
	if err != nil {
		return s.failure(err), nil
	}
	now := time.Now()
	r := plugin.Result{Metrics: plugin.NaNs(2), Time: now.UTC()}
	v, ok := vals[oid]
	if !ok {
		r.Status = plugin.Unknown
		r.Output = fmt.Sprintf("%s: the agent has no object %s", s.addr, oid)
		return r, nil
	}
	shown := text(v)
	n, numeric := number(v)

	switch {
	case cfg.Kind == "counter":
		bits := counterBits(v.Type)
		if bits == 0 {
			r.Status = plugin.Unknown
			r.Output = fmt.Sprintf("%s = %s is a %s, not a counter", oid, shown, v.Type)
			return r, nil
		}
		uptime, _ := uptimeSeconds(vals)
		cur := sample{Value: n, Bits: bits, At: now, Uptime: uptime}
		prev, had := loadSample(t.State, "counter")
		saveSample(t.State, "counter", cur)
		if !had {
			r.Output = fmt.Sprintf("%s = %s (first sample; the rate follows from the next run)", oid, shown)
			break
		}
		if rt, ok := rate(prev, cur, 0); ok {
			r.Metrics[gRate] = rt * cfg.Scale
			r.Output = fmt.Sprintf("%s rate %s/s", oid, fmtNum(r.Metrics[gRate]))
		} else {
			r.Output = fmt.Sprintf("%s = %s (counter reset or agent restart; rate skipped)", oid, shown)
		}
	case numeric:
		r.Metrics[gValue] = n * cfg.Scale
		r.Output = fmt.Sprintf("%s = %s", oid, fmtNum(r.Metrics[gValue]))
	default:
		r.Output = fmt.Sprintf("%s = %q", oid, shown)
	}

	if cfg.Expect != "" {
		if !matches(cfg.Match, cfg.Expect, shown) {
			r.Status = plugin.Critical
			r.Output = fmt.Sprintf("%s = %q, expected %s %q", oid, shown, cfg.Match, cfg.Expect)
		}
	}
	return r, nil
}

func counterBits(t gosnmp.Asn1BER) int {
	switch t {
	case gosnmp.Counter32:
		return 32
	case gosnmp.Counter64:
		return 64
	}
	return 0
}

func matches(mode, expect, got string) bool {
	switch mode {
	case "contains":
		return strings.Contains(got, expect)
	case "regex":
		re, err := regexp.Compile(expect)
		return err == nil && re.MatchString(got)
	default:
		return got == expect
	}
}

// fmtNum prints a value with at most three decimals.
func fmtNum(f float64) string {
	return strconv.FormatFloat(math.Round(f*1000)/1000, 'f', -1, 64)
}
