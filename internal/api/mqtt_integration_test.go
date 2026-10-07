package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mochi-mqtt/server/v2/packets"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/mqtt"
	"github.com/plusclouds/monitoring.server/internal/runner"
)

// mqttClient is a minimal MQTT 3.1.1 client: connect and QoS 1 publish.
type mqttClient struct {
	t    *testing.T
	conn net.Conn
	id   uint16
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

// mqttConnect returns the client and the CONNACK return code (0 = accepted).
func mqttConnect(t *testing.T, addr, user, pass, clientID string, keepalive uint16) (*mqttClient, byte) {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Connect}, ProtocolVersion: 4,
		Connect: packets.ConnectParams{ProtocolName: []byte("MQTT"), Clean: true, Keepalive: keepalive, ClientIdentifier: clientID,
			UsernameFlag: true, Username: []byte(user), PasswordFlag: true, Password: []byte(pass)}}
	var buf bytes.Buffer
	if err := pk.ConnectEncode(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	ack := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, ack); err != nil {
		return &mqttClient{t: t, conn: conn}, 0xFF // closed without CONNACK
	}
	return &mqttClient{t: t, conn: conn}, ack[3]
}

// publish sends QoS 1 and waits for the PUBACK; false when the broker
// closed the connection instead (a topic the credential may not use).
func (c *mqttClient) publish(topic, payload string) bool {
	c.t.Helper()
	c.id++
	pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 1}, TopicName: topic,
		Payload: []byte(payload), PacketID: c.id}
	var buf bytes.Buffer
	if err := pk.PublishEncode(&buf); err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.conn.Write(buf.Bytes()); err != nil {
		return false
	}
	ack := make([]byte, 4)
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c.conn, ack); err != nil {
		return false
	}
	return ack[0] == 0x40
}

// M5 (F08, F12): FixLean sensors connect with the shared credential, are
// registered on first contact, learn their fields, report inventory and
// go offline on disconnect; credentials, topics and limits hold.
func TestMQTTBroker(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	legacyPass := "fl-" + uuid.NewString() // stands in for the firmware's existing shared password
	shared := x.must(x.do("POST", "/v1/ingest/mqtt-credentials", x.key, map[string]any{"name": "fixlean", "kind": "shared",
		"username": "fixlean-site1", "password": legacyPass, "profile": "fixlean-esp", "allow_plain": true}), 201)
	if shared.body["password"] != legacyPass {
		t.Fatalf("created: %s", shared.raw)
	}
	dev := x.must(x.do("POST", "/v1/ingest/mqtt-credentials", x.key, map[string]any{"name": "one", "kind": "device",
		"device_key": "aabbccddee01"}), 201)
	devUser, devPass := dev.body["username"].(string), dev.body["password"].(string)
	if dev.body["device_key"] != "AABBCCDDEE01" || devPass == "" {
		t.Fatalf("device credential: %s", dev.raw)
	}
	if l := x.must(x.do("GET", "/v1/ingest/mqtt-credentials", x.key, nil), 200); strings.Contains(l.raw, legacyPass) {
		t.Error("password listed")
	}

	results := make(chan runner.Result, 200)
	cfg := config.Default().Ingest.MQTT
	cfg.CredentialCacheTTL = config.Duration(time.Millisecond) // see rotations and disabling at once
	tlsAddr, plainAddr := freeAddr(t), freeAddr(t)
	br, err := mqtt.New(mqtt.Options{Config: cfg, NodeID: "node-a", System: db.System, Results: results, NoTLS: true,
		TLSListen: tlsAddr, PlainListen: plainAddr, Logger: slog.New(slog.DiscardHandler), Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	bctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- br.Run(bctx) }()
	t.Cleanup(func() { stop(); <-done })
	for i := 0; ; i++ {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", tlsAddr)
		if err == nil {
			_ = c.Close()
			break
		}
		if i > 50 {
			t.Fatal("broker did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	w, err := metrics.NewWriter(metrics.WriterOptions{System: db.System})
	if err != nil {
		t.Fatal(err)
	}
	apply := func() int {
		t.Helper()
		n := 0
		for {
			select {
			case r := <-results:
				if _, err := x.eng.Apply(ctx, r); err != nil {
					t.Fatal(err)
				}
				w.Add(r)
				n++
			default:
				if err := w.Flush(ctx); err != nil {
					t.Fatal(err)
				}
				return n
			}
		}
	}

	// Credentials and listeners.
	if _, rc := mqttConnect(t, tlsAddr, "fixlean-site1", "wrong", "x", 30); rc == 0 {
		t.Error("wrong password accepted")
	}
	if _, rc := mqttConnect(t, plainAddr, devUser, devPass, "y", 30); rc == 0 {
		t.Error("device credential on the plain listener")
	}

	// A FixLean sensor on the plain listener with the shared password.
	sensor, rc := mqttConnect(t, plainAddr, "fixlean-site1", legacyPass, "246F28AABBCC-1a2b", 60)
	if rc != 0 {
		t.Fatalf("connect: %d", rc)
	}
	status := `{"Product":"FL-VIB-3","Version":"2.4.1","RSSI":-61,"Local IP":"10.1.2.3","Runtime MS":5000,"unixtimestamp":` + itoa(time.Now().Unix()) + `}`
	if !sensor.publish("fixlean/246F28AABBCC/status", status) {
		t.Fatal("status publish")
	}
	var devID, dataCheck, connCheck string
	if err := db.System.QueryRow(ctx, `SELECT device_id::text, data_check_id::text, connection_check_id::text FROM mqtt_devices
		WHERE device_key = '246F28AABBCC'`).Scan(&devID, &dataCheck, &connCheck); err != nil {
		t.Fatalf("not registered: %v", err)
	}
	d := x.must(x.do("GET", "/v1/devices/"+devID, x.key, nil), 200).body
	inv, _ := d["inventory"].(map[string]any)
	if d["name"] != "246F28AABBCC" || d["type"] != "sensor" || inv["firmware"] != "2.4.1" || inv["vendor"] != "fixlean" {
		t.Errorf("registered device: %v", d)
	}
	b := x.must(x.do("GET", "/v1/devices/"+devID+"/mqtt", x.key, nil), 200).body
	if s, _ := b["session"].(map[string]any); s == nil || s["client_id"] != "246F28AABBCC-1a2b" || s["keepalive"] != float64(60) {
		t.Errorf("session: %v", b)
	}
	if c := x.must(x.do("GET", "/v1/checks/"+connCheck, x.key, nil), 200).body; c["is_host_check"] != true || c["plugin"] != "mqtt.connection" {
		t.Errorf("connection check: %v", c)
	}
	if c := x.must(x.do("GET", "/v1/checks/"+dataCheck, x.key, nil), 200).body; c["interval_seconds"] != float64(90) {
		t.Errorf("data check silence 1.5 × keepalive: %v", c["interval_seconds"])
	}

	// Telemetry: new fields are learned and stored; the connection turns OK with data.
	if !sensor.publish("fixlean/246F28AABBCC/telemetry", `{"vrms_x":1.25,"temperature":41.5,"sampling_rate":3200}`) {
		t.Fatal("telemetry publish")
	}
	apply()
	cfgFields := x.must(x.do("GET", "/v1/checks/"+dataCheck, x.key, nil), 200).body["config"].(map[string]any)["fields"]
	if f, _ := cfgFields.([]any); len(f) != 2 || f[0] != "temperature" || f[1] != "vrms_x" {
		t.Errorf("discovered fields: %v", cfgFields)
	}
	st := x.must(x.do("GET", "/v1/checks/"+dataCheck+"/state", x.key, nil), 200).body
	if st["phase"] != "OK" || st["last_metrics"].(map[string]any)["temperature"] != 41.5 {
		t.Errorf("data state: %v", st)
	}
	if st := x.must(x.do("GET", "/v1/checks/"+connCheck+"/state", x.key, nil), 200).body; st["phase"] != "OK" {
		t.Errorf("connection state: %v", st)
	}
	br.Flush(ctx)
	if c := x.must(x.do("GET", "/v1/checks/"+dataCheck, x.key, nil), 200).body; c["push"].(map[string]any)["last_push_at"] == nil {
		t.Errorf("last seen: %v", c["push"])
	}

	// Thresholds on discovered fields work as for any check.
	x.must(x.do("PATCH", "/v1/checks/"+dataCheck, x.key, map[string]any{"thresholds": []any{map[string]any{"metric": "temperature",
		"critical": map[string]any{"op": ">", "value": 60}}}}), 200)
	sensor.publish("fixlean/246F28AABBCC/telemetry", `{"vrms_x":1.3,"temperature":75}`)
	apply()
	if inc := x.incident(dataCheck); inc["severity"] != "critical" {
		t.Errorf("threshold: %v", inc)
	}

	// Pulling the plug: the connection check goes CRITICAL, the session ends.
	_ = sensor.conn.Close()
	select {
	case r := <-results:
		if r.CheckID.String() != connCheck || r.Status.String() != "CRITICAL" {
			t.Errorf("disconnect result: %+v", r)
		}
		if _, err := x.eng.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no disconnect result")
	}
	if b := x.must(x.do("GET", "/v1/devices/"+devID+"/mqtt", x.key, nil), 200).body; b["session"] != nil {
		t.Errorf("session after disconnect: %v", b["session"])
	}
	if a := x.must(x.do("GET", "/v1/devices/"+devID, x.key, nil), 200).body["status"].(map[string]any)["availability"]; a != "down" {
		t.Errorf("availability after disconnect: %v", a)
	}

	// A device credential publishes only under its own key.
	one, rc := mqttConnect(t, tlsAddr, devUser, devPass, "one", 30)
	if rc != 0 {
		t.Fatalf("device connect: %d", rc)
	}
	if !one.publish("sensors/aabbccddee01/data", `{"t":1}`) {
		t.Error("own key refused")
	}
	if one.publish("sensors/OTHER/data", `{"t":1}`) {
		t.Error("other key accepted")
	}
	apply()

	// At the device limit, new keys are dropped and listed.
	if _, err := db.System.Exec(ctx, `UPDATE tenants SET max_devices = (SELECT count(*) FROM devices WHERE tenant_id = $1) WHERE id = $1`,
		x.tenant); err != nil {
		t.Fatal(err)
	}
	gw, _ := mqttConnect(t, tlsAddr, "fixlean-site1", legacyPass, "gw", 30)
	if !gw.publish("fixlean/NEWSENSOR01/t", `{"temperature":20}`) {
		t.Fatal("over-limit publish not acknowledged")
	}
	u := x.must(x.do("GET", "/v1/ingest/unregistered", x.key, nil), 200).body["items"].([]any)
	if len(u) != 1 || u[0].(map[string]any)["device_key"] != "NEWSENSOR01" || !strings.Contains(u[0].(map[string]any)["reason"].(string), "limit") {
		t.Errorf("unregistered: %v", u)
	}
	if _, err := db.System.Exec(ctx, `UPDATE tenants SET max_devices = 100 WHERE id = $1`, x.tenant); err != nil {
		t.Fatal(err)
	}

	// Pre-registration through the API, and the checks it makes.
	manual := x.device("pump-7")
	bound := x.must(x.do("POST", "/v1/devices/"+manual+"/mqtt", x.key, map[string]any{"device_key": "newsensor01", "profile": "json"}), 200).body
	if bound["device_key"] != "NEWSENSOR01" || bound["session"] != nil {
		t.Errorf("bind: %v", bound)
	}
	if u := x.must(x.do("GET", "/v1/ingest/unregistered", x.key, nil), 200).body["items"].([]any); len(u) != 0 {
		t.Errorf("bound key still unregistered: %v", u)
	}
	x.must(x.do("POST", "/v1/devices/"+manual+"/mqtt", x.key, map[string]any{"device_key": "x2"}), 409)
	x.must(x.do("POST", "/v1/devices/"+manual+"/checks", x.key, map[string]any{"name": "m", "plugin": "push.mqtt"}), 422)
	if !gw.publish("fixlean/NEWSENSOR01/t", `{"temperature":21,"env":{"hum":40}}`) {
		t.Fatal("bound publish")
	}
	apply()
	dc := x.must(x.do("GET", "/v1/devices/"+manual+"/mqtt", x.key, nil), 200).body["data_check_id"].(string)
	if f := x.must(x.do("GET", "/v1/checks/"+dc, x.key, nil), 200).body["config"].(map[string]any)["fields"].([]any); len(f) != 2 {
		t.Errorf("json fields: %v", f)
	}
	x.must(x.do("DELETE", "/v1/devices/"+manual+"/mqtt", x.key, nil), 204)
	x.must(x.do("GET", "/v1/checks/"+dc, x.key, nil), 404)
	x.must(x.do("GET", "/v1/devices/"+manual+"/mqtt", x.key, nil), 404)

	// Rotation: the old password stops, the new one works.
	sharedID := x.id(shared)
	rot := x.must(x.do("POST", "/v1/ingest/mqtt-credentials/"+sharedID+"/rotate", x.key, nil), 200).body
	if p, _ := rot["password"].(string); p == "" || p == legacyPass {
		t.Fatalf("rotate: %v", rot)
	}
	time.Sleep(5 * time.Millisecond)
	if _, rc := mqttConnect(t, tlsAddr, "fixlean-site1", legacyPass, "z1", 30); rc == 0 {
		t.Error("old password after rotation")
	}
	if _, rc := mqttConnect(t, tlsAddr, "fixlean-site1", rot["password"].(string), "z2", 30); rc != 0 {
		t.Errorf("new password: %d", rc)
	}
	x.must(x.do("PATCH", "/v1/ingest/mqtt-credentials/"+sharedID, x.key, map[string]any{"enabled": false}), 200)
	time.Sleep(5 * time.Millisecond)
	if _, rc := mqttConnect(t, tlsAddr, "fixlean-site1", rot["password"].(string), "z3", 30); rc == 0 {
		t.Error("disabled credential")
	}
	if p := x.must(x.do("GET", "/v1/ingest/profiles", x.key, nil), 200).body["items"].([]any); len(p) != 2 {
		t.Errorf("profiles: %v", p)
	}
	x.must(x.do("DELETE", "/v1/ingest/mqtt-credentials/"+sharedID, x.key, nil), 204)
}
