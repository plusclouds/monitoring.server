// Package redfish holds the Redfish plugins (F10): redfish.health reads a
// server's hardware health from its BMC (Dell iDRAC, HPE iLO, ASUS and other
// AMI-based BMCs) and reports every component as an object.
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// maxBody is the largest Redfish response read; BMC resources are small.
const maxBody = 4 << 20

// client is a Redfish session against one BMC. It only reads.
type client struct {
	base    *url.URL
	http    *http.Client
	token   string // X-Auth-Token of the session
	session string // session URI, deleted at logout
	user    string
	pass    plugin.Secret
	basic   bool // the BMC has no session service: basic auth per request

	mu    sync.Mutex
	cache map[string]json.RawMessage
}

// authError is a rejected login: a configuration problem (UNKNOWN).
type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

// newClient prepares a client for the device address (host or host:port).
func newClient(t plugin.Target, insecure bool) (*client, error) {
	cred, ok := t.Credentials["auth"]
	if !ok {
		return nil, errors.New(`no Redfish credential: assign a redfish credential in the "auth" role`)
	}
	if t.Address == "" {
		return nil, errors.New("the device has no address")
	}
	base, err := url.Parse("https://" + strings.TrimPrefix(t.Address, "https://"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("invalid BMC address %q", t.Address)
	}
	transport := &http.Transport{
		DialContext:         t.Network.DialContext(15*time.Second, nil),
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure}, //nolint:gosec // default: BMCs ship self-signed certificates; verify_certificate turns checking on
		TLSHandshakeTimeout: 15 * time.Second,
		MaxConnsPerHost:     2, // BMCs are slow and easily overloaded
		IdleConnTimeout:     30 * time.Second,
	}
	return &client{
		base: base, user: cred.Fields["username"], pass: cred.Secret["password"],
		http:  &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		cache: map[string]json.RawMessage{},
	}, nil
}

// login opens a session (X-Auth-Token), so the BMC checks the password once
// per run instead of on every request. BMCs without a session service get
// basic authentication.
func (c *client) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"UserName": c.user, "Password": c.pass.Reveal()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/redfish/v1/SessionService/Sessions"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(res)
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return &authError{fmt.Sprintf("the BMC rejected the login (HTTP %d): check the credential", res.StatusCode)}
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed:
		c.basic = true
		return nil
	case res.StatusCode >= 300:
		return fmt.Errorf("session login: HTTP %d", res.StatusCode)
	}
	c.token = res.Header.Get("X-Auth-Token")
	if c.token == "" {
		c.basic = true
		return nil
	}
	if loc := res.Header.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil {
			c.session = u.Path
		}
	}
	return nil
}

// logout deletes the session even when the run's context is done: BMCs
// allow few sessions, and leaked ones lock monitoring out until they expire.
func (c *client) logout() {
	defer c.http.CloseIdleConnections()
	if c.token == "" || c.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url(c.session), nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Auth-Token", c.token)
	if res, err := c.http.Do(req); err == nil {
		drain(res)
	}
}

func (c *client) url(path string) string {
	u := *c.base
	if p, q, ok := strings.Cut(path, "?"); ok {
		u.Path, u.RawQuery = p, q
	} else {
		u.Path = path
	}
	return u.String()
}

// get reads a resource into out. Responses are cached for the run, since
// several links point to the same resource.
func (c *client) get(ctx context.Context, path string, out any) error {
	path, _, _ = strings.Cut(path, "#") // fragment: a member of an array in the same resource
	c.mu.Lock()
	raw, ok := c.cache[path]
	c.mu.Unlock()
	if !ok {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("OData-Version", "4.0")
		if c.basic {
			req.SetBasicAuth(c.user, c.pass.Reveal())
		} else {
			req.Header.Set("X-Auth-Token", c.token)
		}
		res, err := c.http.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return &authError{fmt.Sprintf("the BMC refused %s (HTTP %d): the account needs at least read-only access", path, res.StatusCode)}
		}
		if res.StatusCode != http.StatusOK {
			return &httpError{path: path, code: res.StatusCode}
		}
		if raw, err = io.ReadAll(io.LimitReader(res.Body, maxBody)); err != nil {
			return err
		}
		c.mu.Lock()
		c.cache[path] = raw
		c.mu.Unlock()
	}
	return json.Unmarshal(raw, out)
}

type httpError struct {
	path string
	code int
}

func (e *httpError) Error() string { return fmt.Sprintf("GET %s: HTTP %d", e.path, e.code) }

// notFound reports a resource the BMC does not have, which is normal for
// optional parts (no storage controller, no Power resource).
func notFound(err error) bool {
	var he *httpError
	return errors.As(err, &he) && (he.code == http.StatusNotFound || he.code == http.StatusNotImplemented)
}

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxBody))
	_ = res.Body.Close()
}

// unreachable reports errors that mean the BMC does not answer, as opposed
// to answering wrongly.
func unreachable(err error) bool {
	var ne net.Error
	var op *net.OpError
	return errors.As(err, &ne) && ne.Timeout() || errors.As(err, &op) || errors.Is(err, context.DeadlineExceeded)
}

// certificateError reports a TLS verification failure.
func certificateError(err error) bool {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	var verify *tls.CertificateVerificationError
	return errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &host) || errors.As(err, &verify)
}
