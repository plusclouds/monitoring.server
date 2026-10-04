package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
)

// Validate checks the config for a `monitor serve` process running roles.
// It reports every problem at once, each prefixed with its key path.
func (c *Config) Validate(roles []string) error {
	var v validator

	v.oneOf("logging.level", c.Logging.Level, "debug", "info", "warn", "error")
	v.oneOf("logging.format", c.Logging.Format, "json", "text")
	v.roles(roles)
	v.oneOf("tls.min_version", c.TLS.MinVersion, "1.2", "1.3")
	v.oneOf("metrics.backend", c.Metrics.Backend, "auto", "postgres", "timescale")
	if c.Usage.DefaultWeight < 0 {
		v.add("usage.default_weight: must not be negative")
	}
	for plugin, w := range c.Usage.Weights {
		if w < 0 || w > 1e6 {
			v.add("usage.weights.%s: must be between 0 and 1000000", plugin)
		}
	}
	v.oneOf("runner.icmp.mode", c.Runner.ICMP.Mode, "auto", "privileged", "unprivileged")
	v.oneOf("crypto.key_provider", c.Crypto.KeyProvider, "file")
	v.cidrs("outbound.deny_networks", c.Outbound.DenyNetworks)

	has := func(r string) bool { return slices.Contains(roles, r) }
	onlyAPI := len(roles) == 1 && has(RoleAPI)

	v.dsn("database.owner", c.Database.Owner, true)
	v.dsn("database.app", c.Database.App, has(RoleAPI))
	v.dsn("database.system", c.Database.System, !onlyAPI)
	v.secretPair("database.listen_dsn", c.Database.ListenDSN, c.Database.ListenDSNFile)

	v.keys(c.Crypto.File)

	if has(RoleAPI) {
		v.required("api.public_url", c.API.PublicURL)
		v.url("api.public_url", c.API.PublicURL)
		v.listener("api", c.API.Listen, c.API.TLS, c.API.TrustedProxies)
	}
	if has(RoleEngine) && c.ProbeService.Enabled {
		v.required("probe_service.public_url", c.ProbeService.PublicURL)
		v.url("probe_service.public_url", c.ProbeService.PublicURL)
		// Probes authenticate with client certificates, so TLS is never optional here.
		if c.ProbeService.TLS.CertFile == "" || c.ProbeService.TLS.KeyFile == "" {
			v.add("probe_service.tls: cert_file and key_file are required")
		}
		for i, t := range c.ProbeService.PresharedEnrollmentTokens {
			key := fmt.Sprintf("probe_service.preshared_enrollment_tokens[%d]", i)
			v.required(key+".name", t.Name)
			v.required(key+".tenant", t.Tenant)
			v.secretRequired(key+".token", t.Token, t.TokenFile)
			v.cidrs(key+".allowed_networks", t.AllowedNetworks)
		}
	}
	if has(RoleIngest) {
		if c.Ingest.HTTP.Enabled {
			v.listener("ingest.http", c.Ingest.HTTP.Listen, c.Ingest.HTTP.TLS, c.Ingest.HTTP.TrustedProxies)
		}
		if c.Ingest.MQTT.Enabled && (c.Ingest.MQTT.TLS.CertFile == "" || c.Ingest.MQTT.TLS.KeyFile == "") {
			v.add("ingest.mqtt.tls: cert_file and key_file are required; plain MQTT is only the legacy listener")
		}
	}

	sm := c.SelfMonitoring
	v.statusServer("self_monitoring", sm.Listen, sm.TLS, sm.StatusToken, sm.StatusTokenFile, false)
	v.cidrs("self_monitoring.allowed_networks", sm.AllowedNetworks)
	for i, t := range sm.Heartbeat.Targets {
		v.url(fmt.Sprintf("self_monitoring.heartbeat.targets[%d].url", i), t.URL)
	}

	return v.err()
}

// Validate checks the config for a `monitor probe` process.
func (c *ProbeConfig) Validate() error {
	var v validator
	v.oneOf("logging.level", c.Logging.Level, "debug", "info", "warn", "error")
	v.oneOf("logging.format", c.Logging.Format, "json", "text")
	v.oneOf("tls.min_version", c.TLS.MinVersion, "1.2", "1.3")
	v.oneOf("runner.icmp.mode", c.Runner.ICMP.Mode, "auto", "privileged", "unprivileged")
	v.required("probe.core_url", c.Probe.CoreURL)
	v.url("probe.core_url", c.Probe.CoreURL)
	v.required("probe.state_dir", c.Probe.StateDir)
	v.secretPair("probe.enroll_token", c.Probe.EnrollToken, c.Probe.EnrollTokenFile)
	if c.HTTP.Enabled {
		// An empty token is fine here: the probe generates one on first start (F09).
		v.statusServer("http", c.HTTP.Listen, c.HTTP.TLS, c.HTTP.StatusToken, c.HTTP.StatusTokenFile, true)
		v.cidrs("http.allowed_networks", c.HTTP.AllowedNetworks)
	}
	return v.err()
}

type validator struct{ errs []error }

func (v *validator) add(format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf(format, args...))
}

func (v *validator) err() error { return errors.Join(v.errs...) }

func (v *validator) required(key, val string) {
	if val == "" {
		v.add("%s is required", key)
	}
}

func (v *validator) oneOf(key, val string, allowed ...string) {
	if !slices.Contains(allowed, val) {
		v.add("%s: %q is not one of %v", key, val, allowed)
	}
}

func (v *validator) url(key, val string) {
	if val == "" {
		return
	}
	u, err := url.Parse(val)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		v.add("%s: %q is not an http(s) URL", key, val)
	}
}

func (v *validator) cidrs(key string, list []string) {
	for _, s := range list {
		if _, err := netip.ParsePrefix(s); err != nil {
			v.add("%s: %q is not a CIDR", key, s)
		}
	}
}

func (v *validator) secretPair(key, inline, file string) {
	if inline != "" && file != "" {
		v.add("%s: set either %s or %s_file, not both", key, key, key)
	}
}

func (v *validator) secretRequired(key, inline, file string) {
	v.secretPair(key, inline, file)
	if inline == "" && file == "" {
		v.add("%s or %s_file is required", key, key)
	}
}

func (v *validator) dsn(key string, d DSN, required bool) {
	v.secretPair(key+".dsn", d.DSN, d.DSNFile)
	if required && d.DSN == "" && d.DSNFile == "" {
		v.add("%s.dsn_file is required", key)
	}
}

func (v *validator) roles(roles []string) {
	if len(roles) == 0 {
		v.add("node.roles: at least one role is required")
	}
	seen := map[string]bool{}
	for _, r := range roles {
		if !slices.Contains(AllRoles, r) {
			v.add("node.roles: unknown role %q", r)
		}
		if seen[r] {
			v.add("node.roles: %q is listed twice", r)
		}
		seen[r] = true
	}
	// Results pass from runner to engine through an in-process channel (ADR-0001).
	if seen[RoleRunner] && !seen[RoleEngine] {
		v.add("node.roles: runner requires engine in the same process")
	}
}

func (v *validator) keys(f FileKeySource) {
	v.required("crypto.file.active", f.Active)
	found := false
	ids := map[string]bool{}
	for i, k := range f.Keys {
		key := fmt.Sprintf("crypto.file.keys[%d]", i)
		v.required(key+".id", k.ID)
		if ids[k.ID] {
			v.add("%s.id: %q is listed twice", key, k.ID)
		}
		ids[k.ID] = true
		if (k.Path == "") == (k.Env == "") {
			v.add("%s: set exactly one of path or env", key)
		}
		if k.ID == f.Active {
			found = true
		}
	}
	if f.Active != "" && !found {
		v.add("crypto.file.active: %q is not in crypto.file.keys", f.Active)
	}
}

// listener checks a public listener: TLS, or plain HTTP only behind a trusted
// proxy or on loopback.
func (v *validator) listener(key, addr string, tls ListenerTLS, trustedProxies []string) {
	v.required(key+".listen", addr)
	v.cidrs(key+".trusted_proxies", trustedProxies)
	if (tls.CertFile == "") != (tls.KeyFile == "") {
		v.add("%s.tls: set both cert_file and key_file", key)
		return
	}
	if tls.CertFile == "" && !isLoopback(addr) && len(trustedProxies) == 0 {
		v.add("%s: plain HTTP on %q needs a loopback address or trusted_proxies that terminate TLS", key, addr)
	}
}

// statusServer checks a /status and /metrics server (F09, F11).
func (v *validator) statusServer(key, addr string, tls ListenerTLS, token, tokenFile string, tokenOptional bool) {
	v.required(key+".listen", addr)
	v.secretPair(key+".status_token", token, tokenFile)
	loopback := isLoopback(addr)
	if tls.Disabled && !loopback {
		v.add("%s.tls.disabled: plain HTTP is only allowed on a loopback address, not %q", key, addr)
	}
	if !tls.Disabled && (tls.CertFile == "") != (tls.KeyFile == "") {
		v.add("%s.tls: set both cert_file and key_file", key)
	}
	if !tokenOptional && !loopback && token == "" && tokenFile == "" {
		v.add("%s.status_token_file is required on a non-loopback address", key)
	}
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
