// Package elasticsearch holds the Elasticsearch plugin: elasticsearch.cluster
// reads a cluster through its REST API and reports the cluster, its nodes and
// its indices as objects.
package elasticsearch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// maxBody bounds an API response; node stats of a large cluster are a few MB.
const maxBody = 32 << 20

const userAgent = "monitor-elasticsearch"

// httpError is a status the cluster answered with.
type httpError struct {
	Path string
	Code int
}

func (e *httpError) Error() string { return fmt.Sprintf("GET %s: HTTP %d", e.Path, e.Code) }

// client talks to one node of a cluster.
type client struct {
	base string // scheme://host:port
	http *http.Client
	cred plugin.Credential
}

// baseURL is where the API is: the device address as host, host:port or
// http(s) URL, with the configured scheme and port filling what it lacks.
func baseURL(addr string, cfg Config) (string, error) {
	addr = strings.TrimSpace(addr)
	scheme := cfg.Scheme
	host := addr
	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", fmt.Errorf("%q is not an http or https URL", addr)
		}
		scheme, host = u.Scheme, u.Host
	}
	if h, p, err := net.SplitHostPort(host); err == nil && p != "" {
		return scheme + "://" + net.JoinHostPort(h, p), nil
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", fmt.Errorf("the device has no address")
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(cfg.Port)), nil
}

func newClient(t plugin.Target, base string, verify bool) *client {
	transport := &http.Transport{
		DialContext:         t.Network.DialContext(15*time.Second, nil),
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !verify}, //nolint:gosec // opt-in: verify_certificate=false for clusters with a private CA
		TLSHandshakeTimeout: 15 * time.Second,
		IdleConnTimeout:     30 * time.Second,
	}
	return &client{base: base, http: &http.Client{Transport: transport}, cred: t.Credentials["auth"]}
}

func (c *client) close() { c.http.CloseIdleConnections() }

// get reads path (with its query) and decodes the JSON answer into out.
func (c *client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	switch c.cred.Type {
	case "":
	case "http_basic":
		req.SetBasicAuth(c.cred.Fields["username"], c.cred.Secret["password"].Reveal())
	case "http_bearer":
		req.Header.Set("Authorization", "Bearer "+c.cred.Secret["token"].Reveal())
	default:
		return fmt.Errorf("credential type %q is not supported by elasticsearch.cluster", c.cred.Type)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return &httpError{Path: path, Code: res.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("GET %s: unreadable response: %w", path, err)
	}
	return nil
}
