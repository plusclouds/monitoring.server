package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, s string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHostCgroupV2(t *testing.T) {
	root := t.TempDir()
	proc, cg := filepath.Join(root, "proc"), filepath.Join(root, "cg")
	write(t, proc, "stat", "cpu  100 0 100 800 0 0 0 0 0 0\n")
	write(t, proc, "loadavg", "0.50 1.00 1.50 1/100 99\n")
	write(t, proc, "uptime", "1234.5 999\n")
	write(t, proc, "meminfo", "MemTotal: 1000 kB\nMemAvailable: 400 kB\nSwapTotal: 100 kB\nSwapFree: 50 kB\n")
	write(t, proc, "net/dev", "Inter-|\n face |\n  lo: 5 0 0 0 0 0 0 0 7 0\neth0: 1000 0 0 0 0 0 0 0 500 0\n")
	write(t, proc, "vmstat", "oom_kill 9\n")
	write(t, cg, "cgroup.controllers", "cpu memory\n")
	write(t, cg, "cpu.stat", "usage_usec 1000000\n")
	write(t, cg, "cpu.max", "200000 100000\n") // 2 CPUs
	write(t, cg, "memory.max", "2000000\n")
	write(t, cg, "memory.current", "1500000\n")
	write(t, cg, "memory.stat", "inactive_file 500000\n")
	write(t, cg, "memory.events", "oom_kill 2\n")

	now := time.Unix(1000, 0)
	c := New(Config{Proc: proc, Cgroup: cg, Disks: []string{root}, Name: "web"})
	c.now = func() time.Time { return now }
	objs, err := c.Collect(context.Background())
	if err != nil || len(objs) != 1 {
		t.Fatalf("%v %v", objs, err)
	}
	m := objs[0].Metrics
	if m["load5"] != 1 || m["uptime_seconds"] != 1234.5 || m["swap_usage_pct"] != 50 || m["oom_kills"] != 2 {
		t.Errorf("first: %v", m)
	}
	// container limit wins over host memory: (1.5M-0.5M)/2M
	if m["mem_usage_pct"] != 50 || m["mem_total_bytes"] != 2000000 {
		t.Errorf("memory: %v", m)
	}
	if _, ok := m["cpu_usage_pct"]; ok {
		t.Error("cpu reported without a previous sample")
	}
	if m["disk_total_bytes"] <= 0 {
		t.Errorf("disk: %v", m)
	}

	// 10 s later: 4 CPU-seconds on 2 CPUs = 20%; 5000 bytes received = 500 B/s.
	now = now.Add(10 * time.Second)
	write(t, cg, "cpu.stat", "usage_usec 5000000\n")
	write(t, proc, "net/dev", "eth0: 6000 0 0 0 0 0 0 0 500 0\n")
	objs, _ = c.Collect(context.Background())
	m = objs[0].Metrics
	if m["cpu_usage_pct"] != 20 || m["net_rx_bytes_per_sec"] != 500 || m["net_tx_bytes_per_sec"] != 0 {
		t.Errorf("second: %v", m)
	}
}

func TestHostProcStatCPU(t *testing.T) {
	root := t.TempDir()
	write(t, root, "stat", "cpu  100 0 100 800 0 0 0 0 0 0\n")
	write(t, root, "loadavg", "1 1 1 1/1 1\n")
	write(t, root, "meminfo", "MemTotal: 1000 kB\nMemAvailable: 500 kB\n")
	now := time.Unix(1000, 0)
	c := New(Config{Proc: root, Cgroup: filepath.Join(root, "none")})
	c.now = func() time.Time { return now }
	_, _ = c.Collect(context.Background())
	now = now.Add(time.Second)
	write(t, root, "stat", "cpu  200 0 200 1000 0 0 0 0 0 0\n") // +200 busy of +400 total
	objs, _ := c.Collect(context.Background())
	if m := objs[0].Metrics; m["cpu_usage_pct"] != 50 || m["mem_usage_pct"] != 50 {
		t.Errorf("%v", m)
	}
	if objs[0].Labels["cgroup"] != "none" {
		t.Errorf("labels %v", objs[0].Labels)
	}
}

func TestHostNothingReadable(t *testing.T) {
	c := New(Config{Proc: t.TempDir(), Cgroup: t.TempDir()})
	if _, err := c.Collect(context.Background()); err == nil {
		t.Error("expected an error with an empty /proc")
	}
}
