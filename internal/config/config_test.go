package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func noEnv(string) (string, bool) { return "", false }

func envMap(m map[string]string) LookupEnv {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// The annotated reference files document the defaults, so loading them must
// change nothing except what they set on purpose (secret file paths, TLS
// files and the KEK list), and must not contain unknown keys.
func TestExampleFilesMatchDefaults(t *testing.T) {
	c, err := Load("../../deploy/config.example.yaml", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	want.Node.ID = c.Node.ID
	want.Database.Owner.DSNFile = c.Database.Owner.DSNFile
	want.Database.App.DSNFile = c.Database.App.DSNFile
	want.Database.System.DSNFile = c.Database.System.DSNFile
	want.Crypto.File = c.Crypto.File
	want.API.TLS = c.API.TLS
	want.ProbeService.TLS = c.ProbeService.TLS
	want.Ingest.HTTP.TLS = c.Ingest.HTTP.TLS
	want.Ingest.MQTT.TLS = c.Ingest.MQTT.TLS
	want.SelfMonitoring.StatusTokenFile = c.SelfMonitoring.StatusTokenFile
	normalizeEmpty(&c)
	normalizeEmpty(&want)
	if !reflect.DeepEqual(c, want) {
		t.Errorf("config.example.yaml differs from Default():\n got %+v\nwant %+v", c, want)
	}

	p, err := LoadProbe("../../deploy/probe.example.yaml", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	wantP := DefaultProbe()
	wantP.Probe.CoreURL = p.Probe.CoreURL
	wantP.Probe.Name = p.Probe.Name
	wantP.Probe.Site = p.Probe.Site
	wantP.Probe.EnrollTokenFile = p.Probe.EnrollTokenFile
	wantP.HTTP.StatusTokenFile = p.HTTP.StatusTokenFile
	if err := p.Validate(); err != nil {
		t.Errorf("probe.example.yaml does not validate: %v", err)
	}
	normalizeEmpty(&p)
	normalizeEmpty(&wantP)
	if !reflect.DeepEqual(p, wantP) {
		t.Errorf("probe.example.yaml differs from DefaultProbe():\n got %+v\nwant %+v", p, wantP)
	}
}

// normalizeEmpty turns empty slices into nil so "[]" in YAML equals an unset default.
func normalizeEmpty(v any) {
	walk(reflect.ValueOf(v).Elem(), nil, func(_ []string, f reflect.Value, _ reflect.StructField) {
		if f.Kind() == reflect.Slice && f.Len() == 0 {
			f.Set(reflect.Zero(f.Type()))
		}
	})
}

func TestUnknownKeyFails(t *testing.T) {
	path := writeFile(t, "logging:\n  lvl: debug\n")
	if _, err := Load(path, noEnv); err == nil || !strings.Contains(err.Error(), "lvl") {
		t.Fatalf("want unknown key error, got %v", err)
	}
}

func TestEnvOverrides(t *testing.T) {
	c, err := Load("", envMap(map[string]string{
		"MONITOR_NODE__ID":                               "core-1",
		"MONITOR_NODE__ROLES":                            "api, notifier",
		"MONITOR_DATABASE__APP__DSN":                     "postgres://app@db/monitor",
		"MONITOR_DATABASE__APP__MAX_CONNS":               "7",
		"MONITOR_API__MAX_BODY":                          "8MiB",
		"MONITOR_API__REQUEST_TIMEOUT":                   "2m",
		"MONITOR_NOTIFIER__RETRY_SCHEDULE":               "1s,1m",
		"MONITOR_RUNNER__TIMEOUTS__FRACTION_OF_INTERVAL": "0.5",
		"MONITOR_INGEST__MQTT__LEGACY__ENABLED":          "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"node.id", c.Node.ID, "core-1"},
		{"node.roles", c.Node.Roles, []string{"api", "notifier"}},
		{"database.app.dsn", c.Database.App.DSN, "postgres://app@db/monitor"},
		{"database.app.max_conns", c.Database.App.MaxConns, 7},
		{"api.max_body", c.API.MaxBody, ByteSize(8 << 20)},
		{"api.request_timeout", c.API.RequestTimeout.D(), 2 * time.Minute},
		{"notifier.retry_schedule", c.Notifier.RetrySchedule, []Duration{Duration(time.Second), Duration(time.Minute)}},
		{"runner.timeouts.fraction_of_interval", c.Runner.Timeouts.FractionOfInterval, 0.5},
		{"ingest.mqtt.legacy.enabled", c.Ingest.MQTT.Legacy.Enabled, true},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
}

func TestEnvOverrideErrors(t *testing.T) {
	_, err := Load("", envMap(map[string]string{
		"MONITOR_API__MAX_BODY":                              "4MB",
		"MONITOR_PROBE_SERVICE__PRESHARED_ENROLLMENT_TOKENS": "x",
	}))
	if err == nil {
		t.Fatal("want errors")
	}
	for _, s := range []string{"MONITOR_API__MAX_BODY", "only be set in the config file"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %q", err, s)
		}
	}
}

func TestByteSize(t *testing.T) {
	for in, want := range map[string]ByteSize{"512": 512, "64KiB": 64 << 10, "4MiB": 4 << 20, "1GiB": 1 << 30} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
		if got.String() != in && in != "512" {
			t.Errorf("%d.String() = %q, want %q", got, got.String(), in)
		}
	}
	for _, bad := range []string{"4MB", "1kB", "-1", "x"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) should fail", bad)
		}
	}
}

func TestReadSecret(t *testing.T) {
	path := writeFile(t, "s3cret\n")
	if got, err := ReadSecret("x", "", path); err != nil || got != "s3cret" {
		t.Errorf("file: got %q, %v", got, err)
	}
	if got, err := ReadSecret("x", "inline", ""); err != nil || got != "inline" {
		t.Errorf("inline: got %q, %v", got, err)
	}
	if _, err := ReadSecret("x", "inline", path); err == nil {
		t.Error("both set should fail")
	}
}

func validBase() Config {
	c := Default()
	c.Node.ID = "n1"
	c.Database.Owner.DSNFile = "/s/owner"
	c.Database.App.DSNFile = "/s/app"
	c.Database.System.DSNFile = "/s/system"
	c.Crypto.File = FileKeySource{Active: "k1", Keys: []KEKRef{{ID: "k1", Path: "/s/k1"}}}
	c.API.PublicURL = "https://monitor.example.com"
	c.API.TLS = ListenerTLS{CertFile: "/c", KeyFile: "/k"}
	c.ProbeService.PublicURL = "https://monitor.example.com:8444"
	c.ProbeService.TLS = ListenerTLS{CertFile: "/c", KeyFile: "/k"}
	c.Ingest.HTTP.TLS = ListenerTLS{CertFile: "/c", KeyFile: "/k"}
	c.Ingest.MQTT.TLS = ListenerTLS{CertFile: "/c", KeyFile: "/k"}
	return c
}

func TestValidate(t *testing.T) {
	if c := validBase(); c.Validate(AllRoles) != nil {
		err := c.Validate(AllRoles)
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := []struct {
		name   string
		roles  []string
		mutate func(*Config)
		want   string
	}{
		{"runner without engine", []string{"runner"}, func(*Config) {}, "runner requires engine"},
		{"unknown role", []string{"api", "web"}, func(*Config) {}, `unknown role "web"`},
		{"plain api on public address", AllRoles, func(c *Config) { c.API.TLS = ListenerTLS{} }, "api: plain HTTP"},
		{"plain api behind proxy", AllRoles, func(c *Config) {
			c.API.TLS = ListenerTLS{}
			c.API.TrustedProxies = []string{"10.0.0.0/8"}
		}, ""},
		{"api-only needs no system DSN", []string{"api"}, func(c *Config) { c.Database.System.DSNFile = "" }, ""},
		{"worker needs system DSN", []string{"notifier"}, func(c *Config) { c.Database.System.DSNFile = "" }, "database.system.dsn_file is required"},
		{"inline and file DSN", AllRoles, func(c *Config) { c.Database.App.DSN = "x" }, "database.app.dsn: set either"},
		{"active KEK missing", AllRoles, func(c *Config) { c.Crypto.File.Active = "k2" }, `"k2" is not in crypto.file.keys`},
		{"KEK path and env", AllRoles, func(c *Config) { c.Crypto.File.Keys[0].Env = "E" }, "exactly one of path or env"},
		{"public status server needs token", AllRoles, func(c *Config) {
			c.SelfMonitoring.Listen = ":9090"
			c.SelfMonitoring.TLS = ListenerTLS{CertFile: "/c", KeyFile: "/k"}
		}, "self_monitoring.status_token_file is required"},
		{"plain status server off loopback", AllRoles, func(c *Config) {
			c.SelfMonitoring.Listen = "0.0.0.0:9090"
			c.SelfMonitoring.StatusTokenFile = "/s/t"
		}, "plain HTTP is only allowed on a loopback address"},
		{"bad deny network", AllRoles, func(c *Config) { c.Outbound.DenyNetworks = []string{"nope"} }, `"nope" is not a CIDR`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validBase()
			tc.mutate(&c)
			err := c.Validate(tc.roles)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRedacted(t *testing.T) {
	c := validBase()
	c.Database.App.DSN = "postgres://app:pw@db"
	c.ProbeService.PresharedEnrollmentTokens = []PresharedEnrollment{{Name: "a", Token: "tok"}}
	r := Redacted(c)
	if r.Database.App.DSN != "[redacted]" || r.ProbeService.PresharedEnrollmentTokens[0].Token != "[redacted]" {
		t.Errorf("secrets not redacted: %+v", r.Database.App)
	}
	if c.Database.App.DSN == "[redacted]" || c.ProbeService.PresharedEnrollmentTokens[0].Token != "tok" {
		t.Error("Redacted modified the original")
	}
	if r.Database.App.DSNFile != "/s/app" {
		t.Error("file paths should not be redacted")
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
