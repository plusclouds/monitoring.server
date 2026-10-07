package vmagent

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Sample is the agent's telemetry envelope (system.SystemMetrics), as the
// vm.agent repository's structs encode it.
func Sample(vm string, ts int64, sent, recv float64, eth1Up bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"v": 1, "id": "7c1f2e0a-5b3d-4e8a-9f61-2d4c8a1b3e55", "type": "telemetry", "agent_type": "vm", "agent_uuid": vm, "timestamp": ts,
		"payload": map[string]any{
			"cpu":    map[string]any{"usage_pct": 12.5, "core_count": 4, "load_avg": []float64{0.42, 0.38, 0.30}},
			"memory": map[string]any{"total_bytes": 8321499136, "used_bytes": 2147483648, "usage_pct": 25.8},
			"disks": []any{
				map[string]any{"device": "/dev/sda1", "mountpoint": "/", "total_bytes": 21474836480, "used_bytes": 8589934592, "usage_pct": 40.0,
					"io": map[string]any{"read_bytes_per_s": 0, "write_bytes_per_s": 20480, "read_iops": 0, "write_iops": 5, "util_pct": 0.4}},
				map[string]any{"device": "/dev/sdb1", "mountpoint": "/data", "total_bytes": 107374182400, "used_bytes": 96636764160, "usage_pct": 90.0},
			},
			"network": []any{
				map[string]any{"interface": "eth0", "bytes_sent": sent, "bytes_recv": recv, "is_up": true},
				map[string]any{"interface": "eth1", "bytes_sent": 0, "bytes_recv": 0, "is_up": eth1Up},
			},
		},
	})
	return b
}

const vm = "0f6e3b4a-1c2d-4e5f-8a9b-0c1d2e3f4a5b"

func TestDecodeAndObjects(t *testing.T) {
	m, err := Decode(Sample(strings.ToUpper(vm), 1790000000, 1000, 5000, false))
	if err != nil || m.VM != vm || !m.Time.Equal(time.Unix(1790000000, 0)) {
		t.Fatalf("decode: %+v %v", m, err)
	}
	prev := map[string]Counters{}
	objs := Objects(m, prev)
	if len(objs) != 5 || objs[0].Key != "host" || objs[1].Key != "disk:/" || objs[2].Name != "/data" || objs[3].Key != "net:eth0" {
		t.Fatalf("objects: %+v", objs)
	}
	h := objs[0].Metrics
	if h[CPUUsage] != 12.5 || h[CPUCores] != 4 || h[Load5] != 0.38 || h[MemUsage] != 25.8 || !math.IsNaN(h[DiskUsage]) {
		t.Errorf("host: %v", h)
	}
	if d := objs[1].Metrics; d[DiskUsage] != 40 || d[DiskWriteBps] != 20480 || d[DiskUtil] != 0.4 || objs[1].Labels["device"] != "/dev/sda1" {
		t.Errorf("disk: %v", d)
	}
	if !math.IsNaN(objs[2].Metrics[DiskReadBps]) {
		t.Error("io without io")
	}
	if objs[3].Metrics[NetUp] != 1 || !math.IsNaN(objs[3].Metrics[NetRxBps]) {
		t.Errorf("first message has no rate yet: %v", objs[3].Metrics)
	}
	if objs[4].Status != plugin.Warning || objs[4].Metrics[NetUp] != 0 {
		t.Errorf("interface down: %+v", objs[4])
	}
	if s := Summary(m, objs); !strings.Contains(s, "CPU 12.5 %") || !strings.Contains(s, "2 disks, 2 interfaces") || !strings.Contains(s, "down: eth1") {
		t.Errorf("summary: %q", s)
	}

	// 30 s later: 30000 bytes sent and 60000 received is 8000 and 16000 bit/s.
	m2, _ := Decode(Sample(vm, 1790000030, 31000, 65000, true))
	objs = Objects(m2, prev)
	if e := objs[3].Metrics; e[NetTxBps] != 8000 || e[NetRxBps] != 16000 {
		t.Errorf("rates: %v", e)
	}
	// A reboot resets the counters: no negative rate.
	m3, _ := Decode(Sample(vm, 1790000060, 10, 10, true))
	if e := Objects(m3, prev)[3].Metrics; !math.IsNaN(e[NetTxBps]) {
		t.Errorf("counter reset: %v", e)
	}
}

func TestDecodeErrors(t *testing.T) {
	for _, bad := range []string{`nope`, `{"type":"heartbeat","agent_uuid":"` + vm + `"}`, `{"type":"telemetry","agent_uuid":"x"}`,
		`{"type":"telemetry","agent_uuid":"` + vm + `","payload":[1]}`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	a := &Agent{}
	if err := plugin.ValidateConfig(a, json.RawMessage(`{"vm_uuid":"`+vm+`"}`)); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{`{}`, `{"vm_uuid":"nope"}`, `{"vm_uuid":"` + vm + `","x":1}`} {
		if err := plugin.ValidateConfig(a, json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if rs, err := a.Parse(nil, Sample(vm, 1790000000, 1, 1, true)); err != nil || len(rs[0].Objects) != 5 {
		t.Errorf("parse: %v %v", rs, err)
	}
}

// FuzzDecode: no message crashes the decoder or yields unusable objects.
func FuzzDecode(f *testing.F) {
	f.Add(Sample(vm, 1790000000, 1, 2, true))
	f.Add([]byte(`{"type":"telemetry","agent_uuid":"` + vm + `","payload":{"disks":[{"mountpoint":""}],"network":[{}]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Decode(b)
		if err != nil {
			return
		}
		objs := Objects(m, map[string]Counters{})
		if len(objs) == 0 || len(objs) > maxObjects || objs[0].Key != "host" {
			t.Fatalf("%d objects", len(objs))
		}
		for _, o := range objs {
			if len(o.Metrics) != NumMetrics || o.Key == "" {
				t.Fatalf("bad object %+v", o)
			}
		}
	})
}
