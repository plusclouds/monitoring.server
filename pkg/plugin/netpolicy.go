package plugin

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// NetPolicy restricts where a plugin may connect: the SSRF guard (design
// section 8). It is checked on the resolved address of every connection, so
// DNS rebinding cannot reach a denied network.
type NetPolicy struct {
	// Deny is always refused (outbound.deny_networks: cloud metadata, ...).
	Deny []netip.Prefix
	// Allow, when set, is the only set of reachable networks
	// (the tenant's allowed_target_networks).
	Allow []netip.Prefix
	// DenyPrivate refuses RFC 1918 and ULA, loopback, link-local and
	// unspecified addresses when Allow is empty. Set for customer tenants.
	DenyPrivate bool
}

// PolicyError means the policy refused a destination. Plugins report it as
// UNKNOWN: it is a configuration problem, not a failure of the device.
type PolicyError struct{ msg string }

func (e *PolicyError) Error() string { return e.msg }

func denied(format string, args ...any) error { return &PolicyError{fmt.Sprintf(format, args...)} }

// Check returns a *PolicyError when ip may not be reached.
func (p *NetPolicy) Check(ip netip.Addr) error {
	if p == nil {
		return nil
	}
	ip = ip.Unmap()
	for _, d := range p.Deny {
		if d.Contains(ip) {
			return denied("connection to %s is not allowed", ip)
		}
	}
	if len(p.Allow) > 0 {
		for _, a := range p.Allow {
			if a.Contains(ip) {
				return nil
			}
		}
		return denied("%s is outside the tenant's allowed target networks", ip)
	}
	if p.DenyPrivate && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
		return denied("%s is a private or local address, which this tenant may not reach", ip)
	}
	return nil
}

// Dialer returns a dialer that enforces the policy on each resolved address.
func (p *NetPolicy) Dialer(timeout time.Duration, local net.Addr) *net.Dialer {
	return &net.Dialer{Timeout: timeout, LocalAddr: local, Control: p.Control()}
}

// Control returns a net.Dialer Control function that enforces the policy on
// the resolved address of each connection, for libraries that dial on their
// own (SNMP). It returns nil when there is no policy.
func (p *NetPolicy) Control() func(network, address string, c syscall.RawConn) error {
	if p == nil {
		return nil
	}
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		return p.Check(ap.Addr())
	}
}

// DialContext is Dialer(...).DialContext, for http.Transport.
func (p *NetPolicy) DialContext(timeout time.Duration, local net.Addr) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return p.Dialer(timeout, local).DialContext
}
