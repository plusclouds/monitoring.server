package redfish

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/pkg/plugin/plugintest"
)

// The DMTF Redfish mockup server (F10) serves testdata/dell-r740: the DMTF
// rackmount sample trimmed and turned into a Dell PowerEdge with a PSU in
// warning, a failed DIMM, an empty DIMM slot, a failed disk and a degraded
// volume. It runs with HTTPS on a self-signed certificate, as BMCs do.
const mockupImage = "dmtf/redfish-mockup-server:latest"

var (
	mockOnce sync.Once
	mockAddr string
	mockErr  error
)

func bmc(t *testing.T) string {
	t.Helper()
	mockOnce.Do(func() { mockAddr, mockErr = startMockup() })
	if mockErr != nil {
		if os.Getenv("MONITOR_REQUIRE_DB") == "1" {
			t.Fatalf("redfish mockup: %v", mockErr)
		}
		t.Skipf("redfish mockup not available: %v", mockErr)
	}
	return mockAddr
}

func startMockup() (string, error) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "redfish-cert")
	if err != nil {
		return "", err
	}
	if err := selfSigned(dir); err != nil {
		return "", err
	}
	var files []testcontainers.ContainerFile
	for _, f := range []string{"cert.pem", "key.pem"} {
		files = append(files, testcontainers.ContainerFile{HostFilePath: filepath.Join(dir, f), ContainerFilePath: "/certs/" + f, FileMode: 0o644})
	}
	err = filepath.WalkDir("testdata/dell-r740", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("testdata", p)
		files = append(files, testcontainers.ContainerFile{HostFilePath: p, ContainerFilePath: "/mockups/" + filepath.ToSlash(rel), FileMode: 0o644})
		return nil
	})
	if err != nil {
		return "", err
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        mockupImage,
			ExposedPorts: []string{"8000/tcp"},
			Files:        files,
			Cmd:          []string{"-p", "8000", "-D", "/mockups/dell-r740", "-S", "-s", "--cert", "/certs/cert.pem", "--key", "/certs/key.pem"},
			WaitingFor:   wait.ForLog("running Server").WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return "", err
	}
	host, err := c.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := c.MappedPort(ctx, "8000/tcp")
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port.Port()), nil
}

func selfSigned(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "idrac-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

func creds() map[string]plugin.Credential {
	return map[string]plugin.Credential{"auth": {Type: "redfish", Fields: map[string]string{"username": "root"},
		Secret: map[string]plugin.Secret{"password": "calvin-secret"}}}
}

func collect(t *testing.T, cfg any) plugin.Result {
	t.Helper()
	return plugintest.Collect(t, &Health{}, plugintest.Options{Address: bmc(t), Config: cfg, Credentials: creds(), Timeout: 20 * time.Second})
}

func TestHealth(t *testing.T) {
	r := collect(t, nil)
	if r.Status != plugin.OK || r.Objects == nil {
		t.Fatalf("collect: %v %q", r.Status, r.Output)
	}
	objs := map[string]plugin.Object{}
	for _, o := range r.Objects {
		objs[o.Key] = o
	}
	want := map[string]plugin.Status{
		"system:437XR1138R2":          plugin.OK,
		"cpu:CPU1":                    plugin.OK,
		"memory:DIMM1":                plugin.OK,
		"memory:DIMM3":                plugin.Critical,
		"storage:RAID.Integrated.1-1": plugin.OK,
		"drive:Disk.Bay.0":            plugin.OK,
		"drive:Disk.Bay.1":            plugin.Critical,
		"volume:Disk.Virtual.0":       plugin.Warning,
		"temperature:CPU1 Temp":       plugin.OK,
		"fan:BaseBoard System Fan":    plugin.OK,
		"psu:PSU 0":                   plugin.Warning,
		"power:0":                     plugin.OK,
	}
	for key, st := range want {
		o, ok := objs[key]
		if !ok {
			t.Errorf("missing %s; have %v", key, keys(objs))
			continue
		}
		if o.Status != st {
			t.Errorf("%s: status %v, want %v (%q)", key, o.Status, st, o.Output)
		}
	}
	for _, absent := range []string{"memory:DIMM4", "temperature:CPU2 Temp"} {
		if _, ok := objs[absent]; ok {
			t.Errorf("%s is absent or disabled and should not be an object", absent)
		}
	}
	sys := objs["system:437XR1138R2"]
	if sys.Name != "Dell Inc. PowerEdge R740" || sys.Labels["serial"] != "7XQ2KJ3" || sys.Labels["bmc_firmware"] != "7.00.00.171" {
		t.Errorf("system: %+v", sys)
	}
	if v := objs["temperature:CPU1 Temp"].Metrics[mTemperature]; v != 41 {
		t.Errorf("CPU1 temperature %v", v)
	}
	if v := objs["fan:BaseBoard System Fan"].Metrics[mFanRPM]; v != 2100 {
		t.Errorf("fan rpm %v", v)
	}
	if psu := objs["psu:PSU 0"]; psu.Metrics[mWatts] != 325 || psu.Metrics[mHealth] != 1 || psu.Labels["model"] != "499253-B21" {
		t.Errorf("psu: %+v", psu)
	}
	if v := objs["power:0"].Metrics[mWatts]; v != 344 {
		t.Errorf("consumed watts %v", v)
	}
	if d := objs["drive:Disk.Bay.1"]; d.Metrics[mHealth] != 2 || d.Labels["capacity"] != "480 GB" || !math.IsNaN(d.Metrics[mTemperature]) {
		t.Errorf("drive: %+v", d)
	}
	if !strings.HasPrefix(r.Output, "Dell Inc. PowerEdge R740 (serial 7XQ2KJ3):") || !strings.Contains(r.Output, "2 critical") {
		t.Errorf("summary: %q", r.Output)
	}

	r = collect(t, Config{Skip: []string{"memory", "drive", "volume", "storage"}})
	for _, o := range r.Objects {
		if k := o.Labels["kind"]; k == "memory" || k == "drive" || k == "volume" || k == "storage" {
			t.Errorf("skipped kind reported: %s", o.Key)
		}
	}
}

func TestCertificateAndFailures(t *testing.T) {
	r := collect(t, Config{VerifyCertificate: true})
	if r.Status != plugin.Unknown || r.Objects != nil || !strings.Contains(r.Output, "verify_certificate") {
		t.Errorf("self-signed certificate: %v %q", r.Status, r.Output)
	}
	r = plugintest.Collect(t, &Health{}, plugintest.Options{Address: "127.0.0.1", Credentials: creds(),
		Network: &plugin.NetPolicy{DenyPrivate: true}})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "private or local") {
		t.Errorf("policy: %v %q", r.Status, r.Output)
	}
	// Nothing listens there: the BMC does not answer.
	r = plugintest.Collect(t, &Health{}, plugintest.Options{Address: "127.0.0.1:1", Credentials: creds(), Timeout: 3 * time.Second})
	if r.Status != plugin.Critical || r.Objects != nil {
		t.Errorf("unreachable: %v %q", r.Status, r.Output)
	}
	r = plugintest.Collect(t, &Health{}, plugintest.Options{Address: "127.0.0.1"})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "no Redfish credential") {
		t.Errorf("no credential: %v %q", r.Status, r.Output)
	}
}

func TestValidate(t *testing.T) {
	if err := (&Health{}).Validate([]byte(`{"skip":["fans"]}`)); err == nil {
		t.Error("unknown kind accepted")
	}
	if err := (&Health{}).Validate([]byte(`{"skip":["fan","psu"],"verify_certificate":true}`)); err != nil {
		t.Error(err)
	}
}

func keys(m map[string]plugin.Object) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return fmt.Sprint(out)
}
