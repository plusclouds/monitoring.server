package host

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// cgroupVersion is 2 when the unified hierarchy is mounted at Cgroup, 1 for
// the legacy memory controller, 0 when neither is visible.
func (c *Collector) cgroupVersion() int {
	if _, err := os.Stat(filepath.Join(c.cfg.Cgroup, "cgroup.controllers")); err == nil {
		return 2
	}
	if _, err := os.Stat(filepath.Join(c.cfg.Cgroup, "memory")); err == nil {
		return 1
	}
	return 0
}

// readKV reads a "key value" file such as cpu.stat or memory.stat.
func readKV(path string) map[string]float64 {
	f, err := os.Open(path) //nolint:gosec // paths are the configured /proc and cgroup mounts
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	out := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) == 2 {
			if n, err := strconv.ParseFloat(fs[1], 64); err == nil {
				out[fs[0]] = n
			}
		}
	}
	return out
}

func readNum(path string) (float64, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // paths are the configured /proc and cgroup mounts
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	return n, err == nil
}

// cgroupCPUUsage is the cgroup's CPU time in microseconds (v2).
func (c *Collector) cgroupCPUUsage() (float64, bool) {
	kv := readKV(filepath.Join(c.cfg.Cgroup, "cpu.stat"))
	u, ok := kv["usage_usec"]
	return u, ok
}

// cgroupCPULimit is the CPUs the cgroup may use: quota/period when limited,
// else every CPU. Zero when the cgroup has no cpu.max (the root cgroup): the
// caller then uses /proc/stat.
func (c *Collector) cgroupCPULimit() float64 {
	b, err := os.ReadFile(filepath.Join(c.cfg.Cgroup, "cpu.max"))
	if err != nil {
		return 0
	}
	fs := strings.Fields(string(b))
	if len(fs) == 2 && fs[0] != "max" {
		q, e1 := strconv.ParseFloat(fs[0], 64)
		p, e2 := strconv.ParseFloat(fs[1], 64)
		if e1 == nil && e2 == nil && p > 0 {
			return q / p
		}
	}
	return float64(runtime.NumCPU())
}

// cgroupMemory returns the container's memory limit and working set (usage
// without reclaimable file cache). ok is false when there is no limit.
func (c *Collector) cgroupMemory(cgv int) (limit, used float64, ok bool) {
	switch cgv {
	case 2:
		limit, ok = readNum(filepath.Join(c.cfg.Cgroup, "memory.max"))
		if !ok {
			return 0, 0, false
		}
		used, ok = readNum(filepath.Join(c.cfg.Cgroup, "memory.current"))
		if !ok {
			return 0, 0, false
		}
		used -= readKV(filepath.Join(c.cfg.Cgroup, "memory.stat"))["inactive_file"]
	case 1:
		dir := filepath.Join(c.cfg.Cgroup, "memory")
		limit, ok = readNum(filepath.Join(dir, "memory.limit_in_bytes"))
		if !ok || limit >= 1<<60 { // unlimited is a huge number
			return 0, 0, false
		}
		used, ok = readNum(filepath.Join(dir, "memory.usage_in_bytes"))
		if !ok {
			return 0, 0, false
		}
		used -= readKV(filepath.Join(dir, "memory.stat"))["total_inactive_file"]
	default:
		return 0, 0, false
	}
	return limit, max(used, 0), true
}

// oomKills is the cgroup's OOM kill count, else the host's.
func (c *Collector) oomKills(cgv int) (float64, bool) {
	if cgv == 2 {
		if n, ok := readKV(filepath.Join(c.cfg.Cgroup, "memory.events"))["oom_kill"]; ok {
			return n, true
		}
	}
	if cgv == 1 {
		if n, ok := readKV(filepath.Join(c.cfg.Cgroup, "memory", "memory.oom_control"))["oom_kill"]; ok {
			return n, true
		}
	}
	n, ok := readKV(filepath.Join(c.cfg.Proc, "vmstat"))["oom_kill"]
	return n, ok
}
