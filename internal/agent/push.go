package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// MaxBody is the largest report sent: the server refuses more than its
// ingest.http.max_body, 64 KiB by default.
const MaxBody = 60 << 10

// Pusher POSTs reports to the server's ingest URL with the check's token.
type Pusher struct {
	url, token string
	client     *http.Client
	errors     atomic.Uint64
	sleep      func(context.Context, time.Duration)
}

// NewPusher builds a pusher from a validated config.
func NewPusher(cfg Config) (*Pusher, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s has no certificate", cfg.CAFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Pusher{url: cfg.URL, token: cfg.Token, client: &http.Client{Transport: tr, Timeout: 10 * time.Second}, sleep: sleepCtx}, nil
}

// Errors is the number of reports that did not get through, since start.
func (p *Pusher) Errors() uint64 { return p.errors.Load() }

// Send delivers a report. It retries a network error, 429 and 5xx twice with
// a pause (honouring Retry-After up to 10 s); other answers are final. A
// report is current state, so an old one is never queued behind a new one.
func (p *Pusher) Send(ctx context.Context, r Report) error {
	body, err := Encode(r)
	if err != nil {
		p.errors.Add(1)
		return err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt) * time.Second
			var ra *retryAfter
			if errors.As(last, &ra) && ra.d > 0 {
				wait = ra.d
			}
			p.sleep(ctx, wait)
			if ctx.Err() != nil {
				break
			}
		}
		retry, err := p.once(ctx, body)
		if err == nil {
			return nil
		}
		last = err
		if !retry {
			break
		}
	}
	p.errors.Add(1)
	var ra *retryAfter
	if errors.As(last, &ra) {
		return ra.err
	}
	return last
}

type retryAfter struct {
	err error
	d   time.Duration
}

func (r *retryAfter) Error() string { return r.err.Error() }

func (p *Pusher) once(ctx context.Context, body []byte) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode/100 == 2:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		e := fmt.Errorf("server answered %d", resp.StatusCode)
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			return true, &retryAfter{e, min(time.Duration(s)*time.Second, 10*time.Second)}
		}
		return true, e
	default:
		return false, fmt.Errorf("server refused the report: %d %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Encode marshals a report within MaxBody: when it is too big the last
// objects are dropped (the host object is first) and the first object says
// how many.
func Encode(r Report) ([]byte, error) {
	dropped := 0
	r.Objects = append([]Object(nil), r.Objects...) // the caller keeps its slice
	for {
		b, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxBody || len(r.Objects) <= 1 {
			return b, nil
		}
		// Drop in chunks so a huge report does not marshal many times.
		n := max(1, len(r.Objects)/8)
		r.Objects = r.Objects[:len(r.Objects)-n]
		dropped += n
		r.Objects[0].Output = fmt.Sprintf("%d objects left out: report over %d bytes", dropped, MaxBody)
	}
}
