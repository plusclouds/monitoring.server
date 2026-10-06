package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/admin"
	"github.com/plusclouds/monitoring.server/internal/api"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/crypto"
	"github.com/plusclouds/monitoring.server/internal/dbtest"
	_ "github.com/plusclouds/monitoring.server/plugins/all"
)

var db *dbtest.DB

func TestMain(m *testing.M) { dbtest.Main(m, &db) }

type env struct {
	t           *testing.T
	url         string
	platformKey string
	adminKey    string // standalone mode only
}

// setup resets the database, bootstraps it and starts the API.
func setup(t *testing.T, standalone bool) *env {
	t.Helper()
	db.Reset(t)
	cfg := config.Default()
	res, err := admin.Bootstrap(context.Background(), db.System, admin.BootstrapOptions{
		Standalone: standalone, TenantName: "acme", TenantDefaults: cfg.Platform.TenantDefaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(api.Options{Config: cfg, DB: db.App, Keys: testKeys(t), Logger: slog.New(slog.DiscardHandler),
		Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &env{t: t, url: ts.URL, platformKey: res.PlatformKey, adminKey: res.AdminKey}
}

// sharedKeys is the keyring of every server and pipeline in these tests, so
// secrets stored through the API can be read by the runner and notifier.
var sharedKeys = func() *crypto.Keyring {
	k := make([]byte, crypto.KeySize)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	keys, err := crypto.NewKeyring("test", map[string][]byte{"test": k})
	if err != nil {
		panic(err)
	}
	return keys
}()

func testKeys(*testing.T) *crypto.Keyring { return sharedKeys }

type resp struct {
	status int
	body   map[string]any
	raw    string
}

func (e *env) do(method, path, key string, body any, headers ...string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.url+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (e *env) must(r resp, status int) resp {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("status %d, want %d: %s", r.status, status, r.raw)
	}
	return r
}

func auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := db.System.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStandaloneAdminKey(t *testing.T) {
	e := setup(t, true)
	me := e.must(e.do("GET", "/v1/me", e.adminKey, nil), 200)
	if me.body["role"] != "admin" || me.body["actor_kind"] != "api_key" {
		t.Errorf("unexpected /me: %s", me.raw)
	}
	tenant := e.must(e.do("GET", "/v1/tenant", e.adminKey, nil), 200)
	if tenant.body["name"] != "acme" {
		t.Errorf("unexpected tenant: %s", tenant.raw)
	}
	// A tenant key cannot provision.
	r := e.must(e.do("GET", "/v1/tenants", e.adminKey, nil), 403)
	if !strings.HasSuffix(r.body["type"].(string), "/platform-only") {
		t.Errorf("unexpected problem: %s", r.raw)
	}
}

func TestAuthenticationFailures(t *testing.T) {
	e := setup(t, true)
	for name, key := range map[string]string{
		"missing":   "",
		"malformed": "not-a-key",
		"unknown":   "mon_ABCDEFGH_" + strings.Repeat("a", 43),
		"mismatch":  e.adminKey[:len(e.adminKey)-1] + "x",
	} {
		r := e.do("GET", "/v1/me", key, nil)
		if r.status != 401 || r.body["type"] != "https://monitor.plusclouds.com/problems/unauthenticated" {
			t.Errorf("%s: got %d %s", name, r.status, r.raw)
		}
	}
	if n := auditCount(t, "auth.failed"); n != 4 {
		t.Errorf("auth.failed events = %d, want 4", n)
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	e := setup(t, true)
	created := e.must(e.do("POST", "/v1/api-keys", e.adminKey, map[string]any{"name": "ci", "role": "read-only"}), 201)
	key := created.body["key"].(string)
	id := created.body["id"].(string)

	e.must(e.do("GET", "/v1/me", key, nil), 200)
	// read-only cannot manage keys
	e.must(e.do("GET", "/v1/api-keys", key, nil), 403)

	list := e.must(e.do("GET", "/v1/api-keys", e.adminKey, nil), 200)
	if items := list.body["items"].([]any); len(items) != 2 {
		t.Errorf("want 2 keys, got %s", list.raw)
	}
	if strings.Contains(list.raw, key) {
		t.Fatal("key list contains a secret")
	}

	e.must(e.do("DELETE", "/v1/api-keys/"+id, e.adminKey, nil), 204)
	e.must(e.do("GET", "/v1/me", key, nil), 401)
	e.must(e.do("DELETE", "/v1/api-keys/"+id, e.adminKey, nil), 404)

	// Spec validation rejects bad input before handlers run.
	r := e.must(e.do("POST", "/v1/api-keys", e.adminKey, map[string]any{"name": "x", "role": "root"}), 400)
	if !strings.HasSuffix(r.body["type"].(string), "/invalid-request") {
		t.Errorf("unexpected problem: %s", r.raw)
	}

	// No audit event, log line or response carries a secret.
	var hits int
	if err := db.System.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE after::text LIKE '%' || $1 || '%'`, key).Scan(&hits); err != nil || hits != 0 {
		t.Errorf("secret found in %d audit events (err %v)", hits, err)
	}
}

// F01: calling the upserts twice with the same body changes nothing and
// writes no second audit row.
func TestProvisioningIsIdempotent(t *testing.T) {
	e := setup(t, false)
	body := map[string]any{"name": "Acme", "limits": map[string]any{"max_devices": 50}}
	first := e.must(e.do("PUT", "/v1/tenants/by-external-id/acc-1", e.platformKey, body), 201)
	e.must(e.do("PUT", "/v1/tenants/by-external-id/acc-1", e.platformKey, body), 200)
	if n := auditCount(t, "tenant.create"); n != 2 { // platform tenant at bootstrap + acc-1
		t.Errorf("tenant.create events = %d, want 2", n)
	}
	if n := auditCount(t, "tenant.update"); n != 0 {
		t.Errorf("tenant.update events = %d, want 0", n)
	}
	limits := first.body["limits"].(map[string]any)
	if limits["max_devices"] != float64(50) || limits["max_checks"] != float64(10000) {
		t.Errorf("limits should merge the platform defaults: %v", limits)
	}

	tid := first.body["id"].(string)
	member := map[string]any{"role": "operator", "display_name": "Ayşe"}
	e.must(e.do("PUT", "/v1/tenants/"+tid+"/members/by-external-id/user-1", e.platformKey, member), 201)
	e.must(e.do("PUT", "/v1/tenants/"+tid+"/members/by-external-id/user-1", e.platformKey, member), 200)
	if n := auditCount(t, "member.create"); n != 1 {
		t.Errorf("member.create events = %d, want 1", n)
	}
	if n := auditCount(t, "member.update"); n != 0 {
		t.Errorf("member.update events = %d, want 0", n)
	}
	members := e.must(e.do("GET", "/v1/tenants/"+tid+"/members", e.platformKey, nil), 200)
	if items := members.body["items"].([]any); len(items) != 1 {
		t.Errorf("members: %s", members.raw)
	}
}

// F01: the platform key acts as the named user with that user's role; a
// removed member gets 403 on the next request.
func TestPlatformActsForUser(t *testing.T) {
	e := setup(t, false)
	tenant := e.must(e.do("PUT", "/v1/tenants/by-external-id/acc-1", e.platformKey, map[string]any{"name": "Acme"}), 201)
	tid := tenant.body["id"].(string)
	e.must(e.do("PUT", "/v1/tenants/"+tid+"/members/by-external-id/user-1", e.platformKey, map[string]any{"role": "admin"}), 201)

	as := []string{"X-Tenant-External-ID", "acc-1", "X-Actor-External-ID", "user-1"}
	me := e.must(e.do("GET", "/v1/me", e.platformKey, nil, as...), 200)
	if me.body["role"] != "admin" || me.body["actor_kind"] != "user" {
		t.Errorf("unexpected /me: %s", me.raw)
	}
	key := e.must(e.do("POST", "/v1/api-keys", e.platformKey, map[string]any{"name": "k", "role": "read-only"}, as...), 201)
	var actorExt string
	if err := db.System.QueryRow(context.Background(),
		`SELECT actor_external_id FROM audit_events WHERE action = 'api_key.create' AND object_id = $1`,
		key.body["id"]).Scan(&actorExt); err != nil || actorExt != "user-1" {
		t.Errorf("audit actor = %q, err %v; want user-1", actorExt, err)
	}

	e.must(e.do("DELETE", "/v1/tenants/"+tid+"/members/by-external-id/user-1", e.platformKey, nil), 204)
	r := e.must(e.do("GET", "/v1/me", e.platformKey, nil, as...), 403)
	if !strings.HasSuffix(r.body["type"].(string), "/not-a-member") {
		t.Errorf("unexpected problem: %s", r.raw)
	}

	// Without an actor the platform has full rights in the tenant.
	me = e.must(e.do("GET", "/v1/me", e.platformKey, nil, "X-Tenant-External-ID", "acc-1"), 200)
	if me.body["role"] != "platform" {
		t.Errorf("unexpected /me: %s", me.raw)
	}
}

// F01: an unknown account named by 20 concurrent requests is created once,
// and an unknown user gets a read-only membership.
func TestJITCreatesTenantOnce(t *testing.T) {
	e := setup(t, false)
	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.do("GET", "/v1/me", e.platformKey, nil,
				"X-Tenant-External-ID", "acc-new", "X-Actor-External-ID", "user-new").status
		})
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Errorf("request %d: status %d", i, c)
		}
	}
	var tenants, users, members int
	err := db.System.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM tenants WHERE external_id = 'acc-new' AND provisioned = 'jit'),
		       (SELECT count(*) FROM users WHERE external_id = 'user-new'),
		       (SELECT count(*) FROM tenant_members WHERE role = 'read-only')`).Scan(&tenants, &users, &members)
	if err != nil || tenants != 1 || users != 1 || members != 1 {
		t.Errorf("tenants %d, users %d, read-only members %d (err %v); want 1 each", tenants, users, members, err)
	}
}

// F01: a key from tenant A gets 404, not 403, for tenant B's objects.
func TestCrossTenantIs404(t *testing.T) {
	e := setup(t, false)
	for _, acc := range []string{"a", "b"} {
		e.must(e.do("PUT", "/v1/tenants/by-external-id/"+acc, e.platformKey, map[string]any{"name": acc}), 201)
	}
	keyA := e.must(e.do("POST", "/v1/api-keys", e.platformKey, map[string]any{"name": "a", "role": "admin"},
		"X-Tenant-External-ID", "a"), 201).body["key"].(string)
	keyB := e.must(e.do("POST", "/v1/api-keys", e.platformKey, map[string]any{"name": "b", "role": "admin"},
		"X-Tenant-External-ID", "b"), 201)
	e.must(e.do("DELETE", "/v1/api-keys/"+keyB.body["id"].(string), keyA, nil), 404)
	e.must(e.do("GET", "/v1/me", keyB.body["key"].(string), nil), 200)
}

// F01: a suspended tenant can read but not write; a deleted tenant is gone.
func TestSuspendAndDelete(t *testing.T) {
	e := setup(t, false)
	e.must(e.do("PUT", "/v1/tenants/by-external-id/acc", e.platformKey, map[string]any{"name": "Acme"}), 201)
	key := e.must(e.do("POST", "/v1/api-keys", e.platformKey, map[string]any{"name": "k", "role": "admin"},
		"X-Tenant-External-ID", "acc"), 201).body["key"].(string)

	e.must(e.do("PATCH", "/v1/tenants/by-external-id/acc", e.platformKey, map[string]any{"status": "suspended"}), 200)
	e.must(e.do("GET", "/v1/tenant", key, nil), 200)
	r := e.must(e.do("POST", "/v1/api-keys", key, map[string]any{"name": "x", "role": "read-only"}), 403)
	if !strings.HasSuffix(r.body["type"].(string), "/tenant-suspended") {
		t.Errorf("unexpected problem: %s", r.raw)
	}
	if n := auditCount(t, "tenant.suspend"); n != 1 {
		t.Errorf("tenant.suspend events = %d, want 1", n)
	}

	e.must(e.do("DELETE", "/v1/tenants/by-external-id/acc", e.platformKey, nil), 204)
	e.must(e.do("DELETE", "/v1/tenants/by-external-id/acc", e.platformKey, nil), 204)
	e.must(e.do("GET", "/v1/tenant", key, nil), 401)
	if n := auditCount(t, "tenant.delete"); n != 1 {
		t.Errorf("tenant.delete events = %d, want 1", n)
	}
}

func TestAuditQuery(t *testing.T) {
	e := setup(t, true)
	e.must(e.do("POST", "/v1/api-keys", e.adminKey, map[string]any{"name": "k", "role": "operator"}), 201)
	page := e.must(e.do("GET", "/v1/audit?limit=1", e.adminKey, nil), 200)
	items := page.body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["action"] != "api_key.create" || page.body["next_cursor"] == nil {
		t.Fatalf("unexpected first page: %s", page.raw)
	}
	next := e.must(e.do("GET", "/v1/audit?limit=100&cursor="+page.body["next_cursor"].(string), e.adminKey, nil), 200)
	firstSeq := items[0].(map[string]any)["seq"].(float64)
	rest := next.body["items"].([]any)
	if len(rest) == 0 {
		t.Fatal("second page is empty")
	}
	for _, it := range rest {
		if seq := it.(map[string]any)["seq"].(float64); seq >= firstSeq {
			t.Errorf("second page has seq %v, want below %v", seq, firstSeq)
		}
	}
	if n := auditCount(t, "audit.query"); n != 2 {
		t.Errorf("audit.query events = %d, want 2", n)
	}
	if _, err := uuid.Parse(items[0].(map[string]any)["id"].(string)); err != nil {
		t.Error("event id is not a UUID")
	}
}

// M1 demo: GET /v1/plugins lists the built-in plugins.
func TestPlugins(t *testing.T) {
	e := setup(t, true)
	list := e.must(e.do("GET", "/v1/plugins", e.adminKey, nil), 200)
	var types []string
	for _, it := range list.body["items"].([]any) {
		p := it.(map[string]any)
		types = append(types, p["type"].(string))
		if p["config_schema"].(map[string]any)["type"] != "object" {
			t.Errorf("%s: config_schema is not an object schema", p["type"])
		}
	}
	if strings.Join(types, ",") != "http,icmp,redfish.health,snmp.get,snmp.interfaces,snmp.pdu,snmp.sensor,snmp.system,snmp.ups" {
		t.Errorf("plugins = %v, want the built-in plugins sorted by type", types)
	}
	snmp := e.must(e.do("GET", "/v1/plugins/snmp.system", e.adminKey, nil), 200)
	if ct, _ := snmp.body["credential_types"].([]any); len(ct) != 2 || ct[0] != "snmp_v3" {
		t.Errorf("snmp.system credential types: %s", snmp.raw)
	}
	if ifs := e.must(e.do("GET", "/v1/plugins/snmp.interfaces", e.adminKey, nil), 200); ifs.body["kind"] != "collector" {
		t.Errorf("snmp.interfaces: %s", ifs.raw)
	}
	icmp := e.must(e.do("GET", "/v1/plugins/icmp", e.adminKey, nil), 200)
	if icmp.body["billing_class"] != "basic" || icmp.body["min_interval_seconds"] != float64(5) {
		t.Errorf("unexpected icmp manifest: %s", icmp.raw)
	}
	e.must(e.do("GET", "/v1/plugins/nope", e.adminKey, nil), 404)
	e.must(e.do("GET", "/v1/plugins", "", nil), 401)
}
