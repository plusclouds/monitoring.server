package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config of the agent. Every top-level key has an AGENT_* environment
// variable; the environment wins over the file.
type Config struct {
	URL        string            `yaml:"url"`        // AGENT_URL: the check's ingest_url from the API
	Token      string            `yaml:"token"`      // AGENT_TOKEN
	TokenFile  string            `yaml:"token_file"` // AGENT_TOKEN_FILE
	AgentID    string            `yaml:"agent_id"`   // AGENT_ID; default the hostname
	Interval   time.Duration     `yaml:"interval"`   // AGENT_INTERVAL; default 30s
	CAFile     string            `yaml:"ca_file"`    // AGENT_CA_FILE: extra CA for the server's certificate
	Collectors []CollectorConfig `yaml:"collectors"`
}

// CollectorConfig is one entry of collectors: type, optional name, and the
// collector's own options as sibling keys.
type CollectorConfig struct {
	Type    string         `yaml:"type"`
	Name    string         `yaml:"name"`
	Options map[string]any `yaml:",inline"`
}

// MinInterval is the shortest report interval (the plugin's MinInterval).
const MinInterval = 5 * time.Second

// DefaultInterval is the report interval when none is set.
const DefaultInterval = 30 * time.Second

// LoadConfig reads the file (empty path: none), applies the environment and
// validates. env nil means the process environment.
func LoadConfig(path string, env func(string) (string, bool)) (Config, error) {
	if env == nil {
		env = os.LookupEnv
	}
	c := Config{Interval: DefaultInterval}
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // the operator chooses the config file
		if err != nil {
			return c, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return c, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	set := func(dst *string, key string) {
		if v, ok := env(key); ok && v != "" {
			*dst = v
		}
	}
	set(&c.URL, "AGENT_URL")
	set(&c.Token, "AGENT_TOKEN")
	set(&c.TokenFile, "AGENT_TOKEN_FILE")
	set(&c.AgentID, "AGENT_ID")
	set(&c.CAFile, "AGENT_CA_FILE")
	if v, ok := env("AGENT_INTERVAL"); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_INTERVAL: %w", err)
		}
		c.Interval = d
	}
	// AGENT_COLLECTORS=host,redis adds collectors with default options (and
	// AGENT_<TYPE>_<KEY> settings) that the file does not name.
	if v, ok := env("AGENT_COLLECTORS"); ok {
		have := map[string]bool{}
		for _, cc := range c.Collectors {
			have[cc.Type] = true
		}
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" && !have[t] {
				c.Collectors = append(c.Collectors, CollectorConfig{Type: t})
				have[t] = true
			}
		}
	}
	if len(c.Collectors) == 0 {
		c.Collectors = []CollectorConfig{{Type: "host"}}
	}
	if c.TokenFile != "" && c.Token == "" {
		b, err := os.ReadFile(c.TokenFile) //nolint:gosec // the operator names the secret file
		if err != nil {
			return c, fmt.Errorf("read token_file: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
	}
	if c.AgentID == "" {
		h, err := os.Hostname()
		if err != nil {
			return c, errors.New("agent_id is empty and the hostname is unavailable")
		}
		c.AgentID = h
	}
	return c, nil
}

// Validate checks what sending needs; `agent check` skips it.
func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("url (AGENT_URL) must be the check's ingest_url, an http(s) URL")
	}
	if c.Token == "" {
		return errors.New("token (AGENT_TOKEN or AGENT_TOKEN_FILE) is required")
	}
	if c.Interval < MinInterval {
		return fmt.Errorf("interval must be at least %s", MinInterval)
	}
	return nil
}
