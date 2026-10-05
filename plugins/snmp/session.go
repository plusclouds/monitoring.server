// Package snmp holds the SNMP plugins (F10): snmp.system (uptime, CPU and
// memory from vendor profiles), snmp.get (one user-defined OID) and
// snmp.ups (UPS-MIB and APC PowerNet).
//
// Every plugin takes one credential in the "auth" role: snmp_v3 (authPriv
// by default) or snmp_v2c, which a user has to choose explicitly.
package snmp

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Connection settings shared by every SNMP plugin.
type Connection struct {
	Port      int  `json:"port,omitempty" jsonschema:"minimum=1,maximum=65535,default=161,description=UDP port of the agent. A port in the device address takes precedence"`
	TimeoutMS int  `json:"timeout_ms,omitempty" jsonschema:"minimum=100,maximum=30000,description=Timeout of one request. Default: 5000 for SNMPv3 (engine discovery costs a round trip) and 2000 for v2c"`
	Retries   *int `json:"retries,omitempty" jsonschema:"minimum=0,maximum=5,default=2,description=Retries of a request that got no answer"`
}

// Credential types the SNMP plugins accept.
var credentialTypes = []string{"snmp_v3", "snmp_v2c"}

// Well-known OIDs.
const (
	oidSysDescr       = ".1.3.6.1.2.1.1.1.0"
	oidSysObjectID    = ".1.3.6.1.2.1.1.2.0"
	oidSysUpTime      = ".1.3.6.1.2.1.1.3.0"
	oidSysName        = ".1.3.6.1.2.1.1.5.0"
	oidHrSystemUptime = ".1.3.6.1.2.1.25.1.1.0"
)

// errNoCredential is reported as UNKNOWN: the check is not configured.
var errNoCredential = errors.New(`no SNMP credential: assign an snmp_v3 or snmp_v2c credential in the "auth" role`)

// session is a connected SNMP client.
type session struct {
	*gosnmp.GoSNMP
	addr string // host:port, for outputs
}

// open connects to the agent of t. The network policy is checked on the
// resolved address. Errors are configuration problems (UNKNOWN).
func open(ctx context.Context, t plugin.Target, c Connection) (*session, error) {
	cred, ok := t.Credentials["auth"]
	if !ok {
		return nil, errNoCredential
	}
	host, port, err := splitAddress(t.Address, c.Port)
	if err != nil {
		return nil, err
	}
	g := &gosnmp.GoSNMP{
		Target: host, Port: port, Transport: "udp", Context: ctx,
		MaxOids: gosnmp.MaxOids, MaxRepetitions: 25,
		Control: t.Network.Control(),
	}
	retries := 2
	if c.Retries != nil {
		retries = *c.Retries
	}
	g.Retries = retries
	timeout := 2 * time.Second
	switch cred.Type {
	case "snmp_v2c":
		g.Version = gosnmp.Version2c
		g.Community = cred.Secret["community"].Reveal()
	case "snmp_v3":
		timeout = 5 * time.Second
		if err := v3(g, cred); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("credential type %s is not an SNMP credential", cred.Type)
	}
	if c.TimeoutMS > 0 {
		timeout = time.Duration(c.TimeoutMS) * time.Millisecond
	}
	// Every attempt must fit in the check's own timeout, with room to report.
	if dl, ok := ctx.Deadline(); ok {
		budget := time.Until(dl) - 100*time.Millisecond
		timeout = max(min(timeout, budget/time.Duration(retries+1)), 100*time.Millisecond)
	}
	g.Timeout = timeout
	if err := g.Connect(); err != nil {
		var pe *plugin.PolicyError
		if errors.As(err, &pe) {
			return nil, pe
		}
		return nil, fmt.Errorf("connect to %s: %w", net.JoinHostPort(host, strconv.Itoa(int(port))), err)
	}
	return &session{GoSNMP: g, addr: net.JoinHostPort(host, strconv.Itoa(int(port)))}, nil
}

func (s *session) close() { _ = s.Conn.Close() }

func splitAddress(addr string, defPort int) (string, uint16, error) {
	if defPort == 0 {
		defPort = 161
	}
	if addr == "" {
		return "", 0, errors.New("the device has no address")
	}
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		// No port, or a bare IPv6 address.
		return strings.Trim(addr, "[]"), uint16(defPort), nil //nolint:gosec // schema bounds the port
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return "", 0, fmt.Errorf("invalid port in address %q", addr)
	}
	return host, uint16(n), nil
}

var (
	authProtocols = map[string]gosnmp.SnmpV3AuthProtocol{
		"MD5": gosnmp.MD5, "SHA": gosnmp.SHA, "SHA-224": gosnmp.SHA224, "SHA-256": gosnmp.SHA256,
		"SHA-384": gosnmp.SHA384, "SHA-512": gosnmp.SHA512,
	}
	privProtocols = map[string]gosnmp.SnmpV3PrivProtocol{
		"DES": gosnmp.DES, "AES": gosnmp.AES, "AES-192": gosnmp.AES192, "AES-256": gosnmp.AES256,
		"AES-192-C": gosnmp.AES192C, "AES-256-C": gosnmp.AES256C,
	}
)

func v3(g *gosnmp.GoSNMP, cred plugin.Credential) error {
	g.Version = gosnmp.Version3
	g.SecurityModel = gosnmp.UserSecurityModel
	g.ContextName = cred.Fields["context_name"]
	usm := &gosnmp.UsmSecurityParameters{UserName: cred.Fields["username"]}
	level := cred.Fields["security_level"]
	if level == "" {
		level = "authPriv"
	}
	field := func(name, def string) string {
		if v := cred.Fields[name]; v != "" {
			return v
		}
		return def
	}
	switch level {
	case "noAuthNoPriv":
		g.MsgFlags = gosnmp.NoAuthNoPriv
	case "authNoPriv", "authPriv":
		p, ok := authProtocols[field("auth_protocol", "SHA-256")]
		if !ok {
			return fmt.Errorf("unsupported SNMPv3 auth protocol %q", cred.Fields["auth_protocol"])
		}
		usm.AuthenticationProtocol = p
		usm.AuthenticationPassphrase = cred.Secret["auth_password"].Reveal()
		g.MsgFlags = gosnmp.AuthNoPriv
		if level == "authPriv" {
			pp, ok := privProtocols[field("priv_protocol", "AES")]
			if !ok {
				return fmt.Errorf("unsupported SNMPv3 privacy protocol %q", cred.Fields["priv_protocol"])
			}
			usm.PrivacyProtocol = pp
			usm.PrivacyPassphrase = cred.Secret["priv_password"].Reveal()
			g.MsgFlags = gosnmp.AuthPriv
		}
	default:
		return fmt.Errorf("unknown SNMPv3 security level %q", level)
	}
	g.SecurityParameters = usm
	return nil
}

// failure turns a request error into a result. No answer means the agent
// or the device is down (CRITICAL); anything the agent answered with, such
// as an authentication report, is a configuration problem (UNKNOWN).
func (s *session) failure(err error) plugin.Result {
	var pe *plugin.PolicyError
	switch {
	case errors.As(err, &pe):
		return plugin.Result{Status: plugin.Unknown, Output: pe.Error()}
	case isTimeout(err):
		return plugin.Errorf(plugin.Critical, "no SNMP response from %s (agent down, unreachable, or wrong v2c community)", s.addr)
	case isAuth(err):
		return plugin.Errorf(plugin.Unknown, "SNMPv3 authentication with %s failed: check the credential (%v)", s.addr, err)
	default:
		return plugin.Errorf(plugin.Unknown, "SNMP request to %s failed: %v", s.addr, err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "timeout") || strings.Contains(s, "request timed out") ||
		strings.Contains(s, "connection refused")
}

func isAuth(err error) bool {
	s := strings.ToLower(err.Error())
	for _, w := range []string{"wrong digest", "unknown user", "usmstats", "authentic", "decryption", "unknown engine", "not in time window"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// get fetches OIDs in one request and returns the values by OID. Missing
// objects (noSuchObject, noSuchInstance) are left out.
func (s *session) get(oids ...string) (map[string]gosnmp.SnmpPDU, error) {
	pkt, err := s.Get(oids)
	if err != nil {
		return nil, err
	}
	if pkt.Error != gosnmp.NoError {
		return nil, fmt.Errorf("agent returned %s", pkt.Error)
	}
	out := make(map[string]gosnmp.SnmpPDU, len(pkt.Variables))
	for _, v := range pkt.Variables {
		if present(v) {
			out[normalizeOID(v.Name)] = v
		}
	}
	return out, nil
}

// walk returns a table column (or subtree) with GETBULK, or GETNEXT on v1.
func (s *session) walk(root string) ([]gosnmp.SnmpPDU, error) {
	var (
		pdus []gosnmp.SnmpPDU
		err  error
	)
	if s.Version == gosnmp.Version1 {
		pdus, err = s.WalkAll(root)
	} else {
		pdus, err = s.BulkWalkAll(root)
	}
	if err != nil {
		return nil, err
	}
	out := pdus[:0]
	for _, p := range pdus {
		if present(p) {
			p.Name = normalizeOID(p.Name)
			out = append(out, p)
		}
	}
	return out, nil
}

func present(v gosnmp.SnmpPDU) bool {
	return v.Type != gosnmp.NoSuchObject && v.Type != gosnmp.NoSuchInstance && v.Type != gosnmp.EndOfMibView &&
		v.Type != gosnmp.Null
}

// normalizeOID returns an OID with a leading dot, as gosnmp reports them.
func normalizeOID(o string) string {
	if !strings.HasPrefix(o, ".") {
		return "." + o
	}
	return o
}

// number returns the numeric value of a PDU: integers, gauges, counters,
// time ticks, floats, and octet strings holding a decimal number (some
// vendors report values as text).
func number(v gosnmp.SnmpPDU) (float64, bool) {
	switch v.Type {
	case gosnmp.Integer, gosnmp.Counter32, gosnmp.Gauge32, gosnmp.TimeTicks, gosnmp.Counter64, gosnmp.Uinteger32:
		f, _ := new(big.Float).SetInt(gosnmp.ToBigInt(v.Value)).Float64()
		return f, true
	case gosnmp.OpaqueFloat:
		f, ok := v.Value.(float32)
		return float64(f), ok
	case gosnmp.OpaqueDouble:
		f, ok := v.Value.(float64)
		return f, ok
	case gosnmp.OctetString:
		b, ok := v.Value.([]byte)
		if !ok {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimRight(string(b), "\x00")), 64)
		return f, err == nil
	}
	return 0, false
}

// text returns a printable value of a PDU.
func text(v gosnmp.SnmpPDU) string {
	switch v.Type {
	case gosnmp.OctetString:
		if b, ok := v.Value.([]byte); ok {
			return strings.TrimRight(string(b), "\x00")
		}
	case gosnmp.ObjectIdentifier:
		if s, ok := v.Value.(string); ok {
			return normalizeOID(s)
		}
	}
	if f, ok := number(v); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v.Value)
}

// uptimeSeconds reads the system uptime: HOST-RESOURCES hrSystemUptime when
// the agent has it (the operating system's uptime), else sysUpTime (the
// agent's). Both are TimeTicks (1/100 s).
func uptimeSeconds(vals map[string]gosnmp.SnmpPDU) (float64, bool) {
	for _, o := range []string{oidHrSystemUptime, oidSysUpTime} {
		if v, ok := vals[o]; ok {
			if f, ok := number(v); ok {
				return f / 100, true
			}
		}
	}
	return 0, false
}
