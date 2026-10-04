package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// NewSender builds a sender from the process config.
func NewSender(c config.Config) *Sender {
	minTLS := uint16(tls.VersionTLS12)
	if c.TLS.MinVersion == "1.3" {
		minTLS = tls.VersionTLS13
	}
	ua := c.Outbound.UserAgent
	if ua == "" {
		ua = "monitor-webhooks"
	}
	return &Sender{UserAgent: ua, ResponseCapture: int(c.Notifier.ResponseCapture), MinTLS: minTLS}
}

// Sender posts signed messages to endpoints.
type Sender struct {
	UserAgent       string
	ResponseCapture int // bytes of the response body kept for the delivery log
	MinTLS          uint16
}

// Target is where and how to send.
type Target struct {
	URL     string
	Timeout time.Duration
	Signing Signing
	Policy  *plugin.NetPolicy // the tenant's outgoing policy: the SSRF guard
}

// Attempt is the outcome of one send.
type Attempt struct {
	StatusCode int    // 0 when no response
	Response   string // first bytes of the body
	Err        error
}

// OK reports a 2xx response.
func (a Attempt) OK() bool { return a.Err == nil && a.StatusCode >= 200 && a.StatusCode < 300 }

// Gone reports a 410: the receiver asks never to be called again.
func (a Attempt) Gone() bool { return a.Err == nil && a.StatusCode == http.StatusGone }

// Send posts body, signed per Standard Webhooks with id as webhook-id.
// Redirects are not followed: a webhook URL must answer itself.
func (s *Sender) Send(ctx context.Context, t Target, id string, body []byte) Attempt {
	ts := time.Now().Unix()
	sig, err := Sign(t.Signing.Secrets, id, ts, body)
	if err != nil {
		return Attempt{Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return Attempt{Err: err}
	}
	for k, v := range t.Signing.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.UserAgent)
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("webhook-signature", sig)

	minTLS := s.MinTLS
	if minTLS == 0 {
		minTLS = tls.VersionTLS12
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:         t.Policy.DialContext(t.Timeout, nil),
			TLSClientConfig:     &tls.Config{MinVersion: minTLS},
			TLSHandshakeTimeout: t.Timeout,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		var pe *plugin.PolicyError
		if errors.As(err, &pe) {
			return Attempt{Err: fmt.Errorf("blocked by the tenant's network policy: %w", pe)}
		}
		return Attempt{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	limit := int64(max(s.ResponseCapture, 0))
	b, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return Attempt{StatusCode: resp.StatusCode, Response: string(b)}
}

// render fills the per-delivery parts of an event body: data.route, and
// data.links.incident from the URL template for single-incident events.
func render(envelope []byte, route json.RawMessage, incidentURL string, tenant uuid.UUID) ([]byte, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, err
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(env["data"], &data); err != nil {
		return nil, err
	}
	if len(route) == 0 {
		route = json.RawMessage("null")
	}
	data["route"] = route
	if incidentURL != "" {
		var subject, account string
		_ = json.Unmarshal(env["subject"], &subject)
		_ = json.Unmarshal(data["account_id"], &account)
		if _, err := uuid.Parse(subject); err == nil && string(data["object"]) != "null" {
			link := strings.NewReplacer("{incident_id}", subject, "{account_id}", account,
				"{tenant_id}", tenant.String()).Replace(incidentURL)
			data["links"], _ = json.Marshal(map[string]string{"incident": link})
		}
	}
	d, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	env["data"] = d
	return json.Marshal(env)
}

// normalize round-trips a snapshot through JSON so that stored and new
// values compare equal.
func normalize(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
