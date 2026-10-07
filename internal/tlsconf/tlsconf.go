// Package tlsconf turns the process TLS policy (design section 8) into
// crypto/tls configurations, so every listener and client uses the same one.
package tlsconf

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/plusclouds/monitoring.server/internal/config"
)

// defaultSuites is the TLS 1.2 list: ECDHE key exchange with AES-GCM or
// ChaCha20-Poly1305 only. TLS 1.3 suites are fixed by Go.
var defaultSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

// Base returns a tls.Config with the policy's minimum version and suites.
func Base(p config.TLSPolicy) (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12, CipherSuites: defaultSuites}
	switch p.MinVersion {
	case "1.2":
	case "1.3":
		c.MinVersion = tls.VersionTLS13
	default:
		return nil, fmt.Errorf("tls.min_version %q is not 1.2 or 1.3", p.MinVersion)
	}
	if len(p.CipherSuites) > 0 {
		suites, err := parseSuites(p.CipherSuites)
		if err != nil {
			return nil, err
		}
		c.CipherSuites = suites
	}
	return c, nil
}

// Server returns a server configuration with the listener's certificate.
func Server(p config.TLSPolicy, l config.ListenerTLS) (*tls.Config, error) {
	c, err := Base(p)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load certificate: %w", err)
	}
	c.Certificates = []tls.Certificate{cert}
	return c, nil
}

func parseSuites(names []string) ([]uint16, error) {
	known := map[string]uint16{}
	for _, s := range tls.CipherSuites() {
		known[s.Name] = s.ID
	}
	out := make([]uint16, 0, len(names))
	for _, n := range names {
		id, ok := known[n]
		if !ok {
			return nil, fmt.Errorf("tls.cipher_suites: %q is not a secure TLS 1.2 suite known to Go", n)
		}
		out = append(out, id)
	}
	return out, nil
}

// ServerReloading is Server with a certificate that is read again when its
// files change (checked at most once a minute), so a renewed certificate
// (Let's Encrypt) applies to long-running listeners without a restart.
func ServerReloading(p config.TLSPolicy, l config.ListenerTLS) (*tls.Config, error) {
	c, err := Base(p)
	if err != nil {
		return nil, err
	}
	r := &reloader{cert: l.CertFile, key: l.KeyFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	c.GetCertificate = r.get
	return c, nil
}

type reloader struct {
	cert, key string
	mu        sync.Mutex
	current   *tls.Certificate
	modTime   time.Time
	checked   time.Time
}

func (r *reloader) load() error {
	st, err := os.Stat(r.cert)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(r.cert, r.key)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}
	r.current, r.modTime = &cert, st.ModTime()
	return nil
}

func (r *reloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) > time.Minute {
		r.checked = time.Now()
		if st, err := os.Stat(r.cert); err == nil && !st.ModTime().Equal(r.modTime) {
			_ = r.load() // a half-written renewal keeps the old certificate until the next check
		}
	}
	return r.current, nil
}
