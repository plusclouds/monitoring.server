// Package host collects the metrics of the machine or container the agent
// runs in: CPU, load, memory, swap, disk, network, uptime and OOM kills. In a
// container with cgroup limits, CPU and memory are the container's own,
// measured against its limits.
package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

func init() {
	agent.Register("host", func(o agent.Options) (agent.Collector, error) {
		return New(Config{
			Proc:   o.String("proc", "/proc"),
			Cgroup: o.String("cgroup", "/sys/fs/cgroup"),
			Disks:  o.List("disks", []string{"/"}),
			Name:   o.String("hostname", ""),
		}), nil
	})
}

// Config of the host collector. Proc and Cgroup are the mount points, so a
// sidecar can read the host's with `-v /proc:/host/proc:ro`.
type Config struct {
	Proc, Cgroup string
	Disks        []string
	Name         string
}

// Collector implements agent.Collector.
type Collector struct {
	cfg  Config
	now  func() time.Time
	prev *sample
}

// sample is the counters of one run, kept for rates.
type sample struct {
	at                time.Time
	cpuBusy, cpuTotal float64 // /proc/stat jiffies
	cgroupCPUUsec     float64 // cgroup v2 cpu.stat usage_usec, -1 when absent
	netRx, netTx      float64
}

// New builds a collector.
func New(cfg Config) *Collector { return &Collector{cfg: cfg, now: time.Now} }

func (c *Collector) Collect(_ context.Context) ([]agent.Object, error) {
	m := map[string]float64{}
	now := c.now()
	cur := &sample{at: now, cgroupCPUUsec: -1}
	var errs []error

	if busy, total, err := c.readStat(); err != nil {
		errs = append(errs, err)
	} else {
		cur.cpuBusy, cur.cpuTotal = busy, total
	}
	cgv := c.cgroupVersion()
	if cgv == 2 {
		if u, ok := c.cgroupCPUUsage(); ok {
			cur.cgroupCPUUsec = u
		}
	}
	cur.netRx, cur.netTx, _ = c.readNet()

	if p := c.prev; p != nil {
		dt := now.Sub(p.at).Seconds()
		if dt > 0 {
			switch {
			case cur.cgroupCPUUsec >= 0 && p.cgroupCPUUsec >= 0 && c.cgroupCPULimit() > 0:
				m["cpu_usage_pct"] = clamp((cur.cgroupCPUUsec-p.cgroupCPUUsec)/1e6/dt/c.cgroupCPULimit()*100, 0, 100)
			case cur.cpuTotal > p.cpuTotal:
				m["cpu_usage_pct"] = clamp((cur.cpuBusy-p.cpuBusy)/(cur.cpuTotal-p.cpuTotal)*100, 0, 100)
			}
			m["net_rx_bytes_per_sec"] = nonNeg(cur.netRx-p.netRx) / dt
			m["net_tx_bytes_per_sec"] = nonNeg(cur.netTx-p.netTx) / dt
		}
	}
	c.prev = cur

	if l1, l5, l15, err := c.readLoad(); err != nil {
		errs = append(errs, err)
	} else {
		m["load1"], m["load5"], m["load15"] = l1, l5, l15
	}
	if err := c.memory(m, cgv); err != nil {
		errs = append(errs, err)
	}
	c.disk(m)
	if up, err := c.readUptime(); err == nil {
		m["uptime_seconds"] = up
	}
	if oom, ok := c.oomKills(cgv); ok {
		m["oom_kills"] = oom
	}

	name := c.cfg.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	obj := agent.Object{Key: "host", Kind: "host", Name: name, Metrics: m,
		Labels: map[string]string{"hostname": name, "cpus": strconv.Itoa(runtime.NumCPU()), "cgroup": fmt.Sprintf("v%d", cgv)}}
	if cgv == 0 {
		obj.Labels["cgroup"] = "none"
	}
	if len(m) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return []agent.Object{obj}, nil
}

func (c *Collector) readStat() (busy, total float64, err error) {
	f, err := os.Open(filepath.Join(c.cfg.Proc, "stat")) //nolint:gosec // the configured /proc mount
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 5 || fs[0] != "cpu" {
			continue
		}
		var v []float64
		for _, s := range fs[1:] {
			n, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("stat: %w", err)
			}
			v = append(v, n)
		}
		for _, n := range v {
			total += n
		}
		idle := v[3]
		if len(v) > 4 {
			idle += v[4] // iowait
		}
		return total - idle, total, nil
	}
	return 0, 0, errors.New("stat: no cpu line")
}

func (c *Collector) readLoad() (l1, l5, l15 float64, err error) {
	b, err := os.ReadFile(filepath.Join(c.cfg.Proc, "loadavg")) //nolint:gosec // the configured /proc mount
	if err != nil {
		return 0, 0, 0, err
	}
	fs := strings.Fields(string(b))
	if len(fs) < 3 {
		return 0, 0, 0, errors.New("loadavg: short")
	}
	var v [3]float64
	for i := range v {
		if v[i], err = strconv.ParseFloat(fs[i], 64); err != nil {
			return 0, 0, 0, fmt.Errorf("loadavg: %w", err)
		}
	}
	return v[0], v[1], v[2], nil
}

func (c *Collector) readUptime() (float64, error) {
	b, err := os.ReadFile(filepath.Join(c.cfg.Proc, "uptime")) //nolint:gosec // the configured /proc mount
	if err != nil {
		return 0, err
	}
	fs := strings.Fields(string(b))
	if len(fs) < 1 {
		return 0, errors.New("uptime: empty")
	}
	return strconv.ParseFloat(fs[0], 64)
}

// meminfo returns /proc/meminfo in bytes.
func (c *Collector) meminfo() (map[string]float64, error) {
	f, err := os.Open(filepath.Join(c.cfg.Proc, "meminfo")) //nolint:gosec // the configured /proc mount
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fs := strings.Fields(rest)
		if len(fs) == 0 {
			continue
		}
		n, err := strconv.ParseFloat(fs[0], 64)
		if err != nil {
			continue
		}
		if len(fs) > 1 && strings.EqualFold(fs[1], "kB") {
			n *= 1024
		}
		out[k] = n
	}
	return out, nil
}

func (c *Collector) memory(m map[string]float64, cgv int) error {
	mi, err := c.meminfo()
	if err != nil {
		return err
	}
	total, avail := mi["MemTotal"], mi["MemAvailable"]
	if total > 0 {
		m["mem_total_bytes"] = total
		m["mem_used_bytes"] = total - avail
		m["mem_usage_pct"] = (total - avail) / total * 100
	}
	if st := mi["SwapTotal"]; st > 0 {
		m["swap_usage_pct"] = (st - mi["SwapFree"]) / st * 100
	}
	// A container limit replaces the host's memory figures.
	if limit, used, ok := c.cgroupMemory(cgv); ok && limit > 0 {
		m["mem_total_bytes"] = limit
		m["mem_used_bytes"] = used
		m["mem_usage_pct"] = clamp(used/limit*100, 0, 100)
	}
	return nil
}

func (c *Collector) readNet() (rx, tx float64, err error) {
	f, err := os.Open(filepath.Join(c.cfg.Proc, "net", "dev")) //nolint:gosec // the configured /proc mount
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		if name = strings.TrimSpace(name); name == "lo" {
			continue
		}
		fs := strings.Fields(rest)
		if len(fs) < 9 {
			continue
		}
		r, _ := strconv.ParseFloat(fs[0], 64)
		t, _ := strconv.ParseFloat(fs[8], 64)
		rx += r
		tx += t
	}
	return rx, tx, nil
}

func (c *Collector) disk(m map[string]float64) {
	var bestPct float64 = -1
	for _, p := range c.cfg.Disks {
		total, used, err := statfs(p)
		if err != nil || total <= 0 {
			continue
		}
		if pct := used / total * 100; pct > bestPct {
			bestPct = pct
			m["disk_usage_pct"], m["disk_used_bytes"], m["disk_total_bytes"] = pct, used, total
		}
	}
}

func clamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

func nonNeg(v float64) float64 { return max(v, 0) }
