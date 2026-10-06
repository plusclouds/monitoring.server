// Package camera holds the camera plugins (F10): camera.snapshot fetches a
// JPEG snapshot (Hikvision, Dahua, Axis, ONVIF or a given URL) and checks
// the picture: blur, darkness and overexposure, a frozen or covered image,
// and a camera that was moved.
package camera

import (
	"context"
	"crypto/md5" //nolint:gosec // HTTP digest authentication (RFC 7616) with MD5 is what cameras implement
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// maxImage bounds a snapshot; a 4K JPEG is a few MB.
const maxImage = 16 << 20

// httpClient fetches from one camera with basic or digest authentication.
type httpClient struct {
	http       *http.Client
	user, pass string
}

func newHTTP(t plugin.Target, verify bool) *httpClient {
	c := &httpClient{http: &http.Client{
		Transport: &http.Transport{
			DialContext:         t.Network.DialContext(10*time.Second, nil),
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !verify}, //nolint:gosec // cameras ship self-signed certificates; verify_certificate turns checking on
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}}
	if cred, ok := t.Credentials["auth"]; ok {
		c.user, c.pass = cred.Fields["username"], cred.Secret["password"].Reveal()
	}
	return c
}

func (c *httpClient) close() { c.http.CloseIdleConnections() }

// statusError is a non-2xx answer.
type statusError struct {
	url  string
	code int
}

func (e *statusError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.url, e.code) }

// do sends a request, answering a digest or basic challenge once.
func (c *httpClient) do(ctx context.Context, method, url, contentType string, body []byte) ([]byte, string, error) {
	send := func(auth string) (*http.Response, error) {
		var rd io.Reader
		if body != nil {
			rd = strings.NewReader(string(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return c.http.Do(req)
	}
	res, err := send("")
	if err != nil {
		return nil, "", err
	}
	if res.StatusCode == http.StatusUnauthorized && c.user != "" {
		challenge := res.Header.Values("WWW-Authenticate")
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		_ = res.Body.Close()
		auth, err := c.authorize(challenge, method, res.Request.URL.RequestURI())
		if err != nil {
			return nil, "", err
		}
		if res, err = send(auth); err != nil {
			return nil, "", err
		}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, "", &statusError{url: redact(url), code: res.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxImage+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxImage {
		return nil, "", fmt.Errorf("%s: the answer is larger than %d MB", redact(url), maxImage>>20)
	}
	return b, res.Header.Get("Content-Type"), nil
}

// authorize answers a challenge: digest (MD5 or SHA-256, qop=auth) when
// offered, else basic.
func (c *httpClient) authorize(challenges []string, method, uri string) (string, error) {
	for _, ch := range challenges {
		scheme, params, _ := strings.Cut(strings.TrimSpace(ch), " ")
		if !strings.EqualFold(scheme, "Digest") {
			continue
		}
		p := parseParams(params)
		var h func() hash.Hash
		switch strings.ToUpper(p["algorithm"]) {
		case "", "MD5":
			h = md5.New
		case "SHA-256":
			h = sha256.New
		default:
			continue
		}
		sum := func(s string) string { x := h(); x.Write([]byte(s)); return hex.EncodeToString(x.Sum(nil)) }
		cnonce := make([]byte, 8)
		_, _ = rand.Read(cnonce)
		cn, nc := hex.EncodeToString(cnonce), "00000001"
		ha1 := sum(c.user + ":" + p["realm"] + ":" + c.pass)
		ha2 := sum(method + ":" + uri)
		qop := ""
		for _, q := range strings.Split(p["qop"], ",") {
			if strings.TrimSpace(q) == "auth" {
				qop = "auth"
			}
		}
		var resp string
		if qop != "" {
			resp = sum(ha1 + ":" + p["nonce"] + ":" + nc + ":" + cn + ":" + qop + ":" + ha2)
		} else {
			resp = sum(ha1 + ":" + p["nonce"] + ":" + ha2)
		}
		out := fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q, response=%q`, c.user, p["realm"], p["nonce"], uri, resp)
		if p["algorithm"] != "" {
			out += ", algorithm=" + p["algorithm"]
		}
		if qop != "" {
			out += fmt.Sprintf(`, qop=%s, nc=%s, cnonce=%q`, qop, nc, cn)
		}
		if p["opaque"] != "" {
			out += fmt.Sprintf(`, opaque=%q`, p["opaque"])
		}
		return out, nil
	}
	for _, ch := range challenges {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ch)), "basic") {
			return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass)), nil
		}
	}
	return "", errors.New("the camera asks for an authentication scheme it does not support")
}

// parseParams reads key=value and key="value" pairs of a challenge.
func parseParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,")
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				v, s = rest[1:], ""
			} else {
				v, s = rest[1:end+1], rest[end+2:]
			}
		} else {
			v, s, _ = strings.Cut(rest, ",")
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

// redact drops credentials from a URL for outputs.
func redact(u string) string {
	if i := strings.Index(u, "@"); i > 0 {
		if j := strings.Index(u, "://"); j > 0 && j < i {
			return u[:j+3] + u[i+1:]
		}
	}
	return u
}
