package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/ingest"
	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/natsingest"
	"github.com/plusclouds/monitoring.server/internal/runner"
)

func agentTelemetry(vm string, ts int64, dataPct float64) []byte {
	b, _ := json.Marshal(map[string]any{"v": 1, "id": "x", "type": "telemetry", "agent_type": "vm", "agent_uuid": vm, "timestamp": ts,
		"payload": map[string]any{
			"cpu":    map[string]any{"usage_pct": 12.5, "core_count": 4, "load_avg": []float64{0.4, 0.3, 0.2}},
			"memory": map[string]any{"total_bytes": 8e9, "used_bytes": 2e9, "usage_pct": 25},
			"disks": []any{map[string]any{"device": "/dev/sda1", "mountpoint": "/", "total_bytes": 2e10, "used_bytes": 8e9, "usage_pct": 40},
				map[string]any{"device": "/dev/sdb1", "mountpoint": "/data", "total_bytes": 1e11, "used_bytes": 1e11 * dataPct / 100, "usage_pct": dataPct}},
			"network": []any{map[string]any{"interface": "eth0", "bytes_sent": float64(ts) * 1000, "bytes_recv": float64(ts) * 2000, "is_up": true}},
		}})
	return b
}

// vm.agent (PlusClouds VM agent over NATS): the platform creates a device
// and a check per VM; telemetry becomes host, disk and interface objects
// with thresholds; silence is CRITICAL; other VMs' and forged messages are
// dropped.
func TestVMAgent(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	const vm = "0f6e3b4a-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	dev := x.id(x.must(x.do("POST", "/v1/devices", x.key, map[string]any{"name": "web-01", "type": "vm",
		"external": map[string]any{"source": "plusclouds", "type": "vm", "id": vm}}), 201))
	chk := x.id(x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "agent", "plugin": "vm.agent",
		"is_host_check": true, "failure_count": 1, "config": map[string]any{"vm_uuid": strings.ToUpper(vm)},
		"thresholds": []any{map[string]any{"metric": "disk_usage_pct", "object": "*", "warning": map[string]any{"op": ">", "value": 85}}}}), 201))
	// One check per VM on the whole server.
	other := x.device("web-01-copy")
	x.must(x.do("POST", "/v1/devices/"+other+"/checks", x.key, map[string]any{"name": "agent", "plugin": "vm.agent",
		"config": map[string]any{"vm_uuid": vm}}), 409)
	x.must(x.do("POST", "/v1/devices/"+other+"/checks", x.key, map[string]any{"name": "agent", "plugin": "vm.agent",
		"config": map[string]any{"vm_uuid": "nope"}}), 422)

	results := make(chan runner.Result, 20)
	c, err := natsingest.New(natsingest.Options{Config: config.Default().Ingest.NATS, System: db.System, Results: results,
		Logger: slog.New(slog.DiscardHandler), Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
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
	now := time.Now().Unix()
	c.Handle(ctx, "vm."+vm+".telemetry", agentTelemetry(vm, now-30, 50))
	c.Handle(ctx, "vm."+vm+".telemetry", agentTelemetry(vm, now, 90))
	// Another VM's subject with this VM's payload, an unknown VM and junk: dropped.
	c.Handle(ctx, "vm.11111111-2222-3333-4444-555555555555.telemetry", agentTelemetry(vm, now, 99))
	c.Handle(ctx, "vm.11111111-2222-3333-4444-555555555555.telemetry", agentTelemetry("11111111-2222-3333-4444-555555555555", now, 99))
	c.Handle(ctx, "vm."+vm+".telemetry", []byte(`{"type":"heartbeat"}`))
	if n := apply(); n != 2 {
		t.Fatalf("applied %d results, want 2", n)
	}

	objs := x.must(x.do("GET", "/v1/checks/"+chk+"/objects", x.key, nil), 200).body["items"].([]any)
	byKey := map[string]map[string]any{}
	for _, o := range objs {
		m := o.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if len(byKey) != 4 || byKey["host"] == nil || byKey["disk:/data"] == nil || byKey["net:eth0"] == nil {
		t.Fatalf("objects: %v", objs)
	}
	if m := byKey["net:eth0"]["last_metrics"].(map[string]any); m["net_tx_bits_per_s"] != float64(8000) {
		t.Errorf("interface rate: %v", m)
	}
	if inc := x.incident(chk); inc["object_key"] != "disk:/data" || inc["severity"] != "warning" {
		t.Errorf("disk threshold: %v", inc)
	}
	if a := x.must(x.do("GET", "/v1/devices/"+dev, x.key, nil), 200).body["status"].(map[string]any)["availability"]; a != "up" {
		t.Errorf("availability: %v", a)
	}

	// Silence: the VM is off. The last-seen rule turns the host check CRITICAL.
	c.Flush(ctx)
	if p := x.must(x.do("GET", "/v1/checks/"+chk, x.key, nil), 200).body["push"].(map[string]any); p["last_push_at"] == nil {
		t.Errorf("last seen: %v", p)
	}
	if _, err := db.System.Exec(ctx, `UPDATE push_sources SET last_push_at = now() - interval '10 minutes',
		created_at = now() - interval '1 hour' WHERE check_id = $1`, chk); err != nil {
		t.Fatal(err)
	}
	sweep, err := ingest.New(ingest.Options{System: db.System, Results: results, Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := sweep.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	apply()
	if a := x.must(x.do("GET", "/v1/devices/"+dev, x.key, nil), 200).body["status"].(map[string]any)["availability"]; a != "down" {
		t.Errorf("availability after silence: %v", a)
	}
}
