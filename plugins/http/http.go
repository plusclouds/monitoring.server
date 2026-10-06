// Package http is the HTTP(S) check (F10): status code, keyword match and a
// timing breakdown (DNS, connect, TLS, first byte, total).
package http

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Check{}) }

// maxBody is how much of the body is read for keyword matching.
const maxBody = 1 << 20

// Config of an http check.
type Config struct {
	URL                string            `json:"url,omitempty" jsonschema:"format=uri,description=URL to request. Default: the device address"`
	Method             string            `json:"method,omitempty" jsonschema:"enum=GET,enum=HEAD,enum=POST,enum=PUT,enum=OPTIONS,default=GET"`
	Headers            map[string]string `json:"headers,omitempty" jsonschema:"description=Extra request headers"`
	Body               string            `json:"body,omitempty" jsonschema:"maxLength=65536"`
	ExpectedStatus     []int             `json:"expected_status,omitempty" jsonschema:"description=Accepted status codes. Default: 200-399"`
	Keyword            string            `json:"keyword,omitempty" jsonschema:"description=Text that must appear in the first 1 MiB of the body"`
	KeywordAbsent      string            `json:"keyword_absent,omitempty" jsonschema:"description=Text that must not appear in the body"`
	FollowRedirects    *bool             `json:"follow_redirects,omitempty" jsonschema:"default=true"`
	MaxRedirects       int               `json:"max_redirects,omitempty" jsonschema:"minimum=0,maximum=10,default=5"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify,omitempty" jsonschema:"description=Accept invalid certificates. Use the tls check to monitor certificates"`
	SourceIP           string            `json:"source_ip,omitempty" jsonschema:"description=Local address to send from"`
}

var defaults = Config{Method: http.MethodGet, MaxRedirects: 5}

// Metric slots, in manifest order.
const (
	mDNS = iota
	mConnect
	mTLS
	mTTFB
	mTotal
	mStatus
	mBodyBytes
	metricCount
)

// userAgent identifies the check to the monitored site.
const userAgent = "monitor-http-check"

// Check implements plugin.Check.
type Check struct{}

func (c *Check) Manifest() plugin.Manifest {
	gauge := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type:            "http",
		Kind:            plugin.KindCheck,
		Description:     "HTTP(S) request: status code, keyword and timing breakdown.",
		ConfigSchema:    plugin.SchemaFor[Config](),
		CredentialTypes: []string{"http_basic", "http_bearer"},
		Metrics: []plugin.MetricDef{
			gauge("dns_ms", "ms", "DNS lookup"),
			gauge("connect_ms", "ms", "TCP connect"),
			gauge("tls_ms", "ms", "TLS handshake"),
			gauge("ttfb_ms", "ms", "Time to first byte, from the start"),
			gauge("total_ms", "ms", "Whole request including the body read"),
			gauge("status_code", "1", "HTTP status code"),
			gauge("body_bytes", "bytes", "Body bytes read (up to 1 MiB)"),
		},
		DefaultInterval: time.Minute,
		MinInterval:     5 * time.Second,
		BillingClass:    plugin.BillingStandard,
		WhoopsyMetric:   "total_ms",
	}
}

func (c *Check) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, defaults)
	if err != nil {
		return err
	}
	if cfg.URL != "" {
		if _, err := parseURL(cfg.URL); err != nil {
			return err
		}
	}
	if cfg.SourceIP != "" && net.ParseIP(cfg.SourceIP) == nil {
		return fmt.Errorf("source_ip %q is not an IP address", cfg.SourceIP)
	}
	for _, s := range cfg.ExpectedStatus {
		if s < 100 || s > 599 {
			return fmt.Errorf("expected_status %d is not an HTTP status", s)
		}
	}
	return nil
}

func parseURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http or https URL", s)
	}
	return u, nil
}

func (c *Check) Run(ctx context.Context, t plugin.Target) (plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(t.Config, defaults)
	if err != nil {
		return plugin.Result{}, err
	}
	target := cfg.URL
	if target == "" {
		target = t.Address
	}
	u, err := parseURL(target)
	if err != nil {
		return plugin.Result{}, err
	}

	var local net.Addr
	if cfg.SourceIP != "" {
		local = &net.TCPAddr{IP: net.ParseIP(cfg.SourceIP)}
	}
	transport := &http.Transport{
		DialContext:         t.Network.DialContext(30*time.Second, local),
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify}, //nolint:gosec // opt-in per check
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 30 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	defer transport.CloseIdleConnections()
	follow := cfg.FollowRedirects == nil || *cfg.FollowRedirects
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if !follow || len(via) > cfg.MaxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	var body io.Reader
	if cfg.Body != "" {
		body = strings.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(ctx, cfg.Method, u.String(), body)
	if err != nil {
		return plugin.Result{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	if err := authorize(req, t.Credentials["auth"]); err != nil {
		return plugin.Result{}, err
	}

	m := plugin.NaNs(metricCount)
	var dnsStart, connStart, tlsStart time.Time
	start := time.Now()
	since := func(t0 time.Time) float64 { return float64(time.Since(t0).Microseconds()) / 1000 }
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { m[mDNS] = since(dnsStart) },
		ConnectStart:         func(string, string) { connStart = time.Now() },
		ConnectDone:          func(string, string, error) { m[mConnect] = since(connStart) },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { m[mTLS] = since(tlsStart) },
		GotFirstResponseByte: func() { m[mTTFB] = since(start) },
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))

	res, err := client.Do(req)
	if err != nil {
		m[mTotal] = since(start)
		status := plugin.Critical
		if _, ok := errors.AsType[*plugin.PolicyError](err); ok {
			status = plugin.Unknown
		}
		return plugin.Result{Status: status, Output: describe(err), Metrics: m}, nil
	}
	defer func() { _ = res.Body.Close() }()
	b, readErr := io.ReadAll(io.LimitReader(res.Body, maxBody))
	m[mTotal] = since(start)
	m[mStatus] = float64(res.StatusCode)
	m[mBodyBytes] = float64(len(b))

	r := plugin.Result{Status: plugin.OK, Metrics: m}
	var problems []string
	if !statusOK(res.StatusCode, cfg.ExpectedStatus) {
		problems = append(problems, fmt.Sprintf("unexpected status %s", res.Status))
	}
	if readErr != nil {
		problems = append(problems, "body read failed: "+describe(readErr))
	}
	if cfg.Keyword != "" && !strings.Contains(string(b), cfg.Keyword) {
		problems = append(problems, fmt.Sprintf("keyword %q not found", cfg.Keyword))
	}
	if cfg.KeywordAbsent != "" && strings.Contains(string(b), cfg.KeywordAbsent) {
		problems = append(problems, fmt.Sprintf("keyword %q present", cfg.KeywordAbsent))
	}
	if len(problems) > 0 {
		r.Status = plugin.Critical
		r.Output = fmt.Sprintf("%s %s: %s", cfg.Method, u.Redacted(), strings.Join(problems, "; "))
		return r, nil
	}
	r.Output = fmt.Sprintf("%s %s: %s in %.0f ms", cfg.Method, u.Redacted(), res.Status, m[mTotal])
	return r, nil
}

func authorize(req *http.Request, cred plugin.Credential) error {
	switch cred.Type {
	case "":
		return nil
	case "http_basic":
		req.SetBasicAuth(cred.Fields["username"], cred.Secret["password"].Reveal())
	case "http_bearer":
		req.Header.Set("Authorization", "Bearer "+cred.Secret["token"].Reveal())
	default:
		return fmt.Errorf("credential type %q is not supported by the http check", cred.Type)
	}
	return nil
}

func statusOK(code int, expected []int) bool {
	if len(expected) == 0 {
		return code >= 200 && code < 400
	}
	return slices.Contains(expected, code)
}

// describe turns transport errors into short, distinct messages, so test
// connection can tell "refused", "timeout" and TLS problems apart (F02).
func describe(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var opErr *net.OpError
	if pe, ok := errors.AsType[*plugin.PolicyError](err); ok {
		return pe.Error()
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dnsErr):
		return "DNS lookup failed: " + dnsErr.Err
	case errors.As(err, &certErr):
		return "certificate verification failed: " + certErr.Err.Error()
	case errors.As(err, &opErr) && strings.Contains(opErr.Err.Error(), "connection refused"):
		return "connection refused"
	default:
		return err.Error()
	}
}
