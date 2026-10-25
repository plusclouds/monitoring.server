// Package services holds the collectors for software the image runs:
// redis, postgres, elasticsearch, nginx, php-fpm and node.js. Each reports one
// object, "service:<name>", with up, response_ms and what that service
// exposes. A service that does not answer is reported as critical with
// up=0, not as an agent error.
package services

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

func init() {
	agent.Register("redis", newRedis)
	agent.Register("postgres", newPostgres)
	agent.Register("elasticsearch", newElasticsearch)
	agent.Register("nginx", newNginx)
	agent.Register("phpfpm", newPHPFPM)
	agent.Register("nodejs", newNode)
}

// object starts the service object of a collector.
func object(typ, name string, labels map[string]string) agent.Object {
	return agent.Object{Key: "service:" + name, Kind: "service", Name: name, Labels: withKind(typ, labels), Metrics: map[string]float64{}}
}

func withKind(typ string, l map[string]string) map[string]string {
	if l == nil {
		l = map[string]string{}
	}
	l["service"] = typ
	return l
}

// down marks the service as not answering.
func down(o agent.Object, err error) []agent.Object {
	o.Status, o.Output = agent.Critical, trim(err.Error())
	o.Metrics = map[string]float64{"up": 0}
	return []agent.Object{o}
}

func trim(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// httpClient never follows redirects: a redirecting health endpoint is
// reported as it answers.
func httpClient(timeout time.Duration, insecure bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec // opt-in, for self-signed local endpoints
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// rate turns two counter samples into a per-second rate; ok is false on the
// first sample or after a counter reset.
type rate struct {
	at   time.Time
	last float64
	have bool
}

func (r *rate) next(now time.Time, v float64) (float64, bool) {
	prevAt, prev, had := r.at, r.last, r.have
	r.at, r.last, r.have = now, v, true
	dt := now.Sub(prevAt).Seconds()
	if !had || dt <= 0 || v < prev {
		return 0, false
	}
	return (v - prev) / dt, true
}
