package services

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

type redis struct {
	name, addr, user, password string
	timeout                    time.Duration
	now                        func() time.Time
}

func newRedis(o agent.Options) (agent.Collector, error) {
	to, err := o.Duration("timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	addr := o.String("addr", "127.0.0.1:6379")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("addr %q must be host:port", addr)
	}
	return &redis{name: o.Name, addr: addr, user: o.String("username", ""), password: o.String("password", ""), timeout: to, now: time.Now}, nil
}

// Collect speaks RESP inline commands (AUTH, INFO) over a plain TCP
// connection: no client library, no TLS (use a local connection or a tunnel).
func (r *redis) Collect(ctx context.Context) ([]agent.Object, error) {
	obj := object("redis", r.name, map[string]string{"addr": r.addr})
	start := time.Now()
	d := net.Dialer{Timeout: r.timeout}
	conn, err := d.DialContext(ctx, "tcp", r.addr)
	if err != nil {
		return down(obj, err), nil
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(r.timeout))
	br := bufio.NewReader(conn)
	if r.password != "" {
		auth := "AUTH " + r.password
		if r.user != "" {
			auth = "AUTH " + r.user + " " + r.password
		}
		if _, err := fmt.Fprint(conn, auth+"\r\n"); err != nil {
			return down(obj, err), nil
		}
		if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+OK") {
			return down(obj, fmt.Errorf("auth refused: %s", strings.TrimSpace(line))), nil
		}
	}
	if _, err := fmt.Fprint(conn, "INFO\r\n"); err != nil {
		return down(obj, err), nil
	}
	info, err := readBulk(br)
	if err != nil {
		return down(obj, err), nil
	}
	m := parseInfo(info)
	obj.Metrics["up"] = 1
	obj.Metrics["response_ms"] = ms(time.Since(start))
	r.metrics(&obj, m)
	return []agent.Object{obj}, nil
}

// readBulk reads one RESP bulk string, or turns an error reply into an error.
func readBulk(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(line, "-") {
		return "", errors.New(strings.TrimPrefix(line, "-"))
	}
	if !strings.HasPrefix(line, "$") {
		return "", fmt.Errorf("unexpected reply %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil || n < 0 || n > 4<<20 {
		return "", fmt.Errorf("bad bulk length %q", line)
	}
	buf := make([]byte, n+2)
	if _, err := readFull(br, buf); err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := br.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func parseInfo(s string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			m[k] = v
		}
	}
	return m
}

func (r *redis) metrics(obj *agent.Object, m map[string]string) {
	num := func(k string) (float64, bool) {
		v, err := strconv.ParseFloat(m[k], 64)
		return v, err == nil
	}
	set := func(dst, k string) {
		if v, ok := num(k); ok {
			obj.Metrics[dst] = v
		}
	}
	set("connections", "connected_clients")
	set("redis_blocked_clients", "blocked_clients")
	set("mem_used_bytes", "used_memory")
	set("uptime_seconds", "uptime_in_seconds")
	set("redis_evicted_keys", "evicted_keys")
	set("ops_per_sec", "instantaneous_ops_per_sec")
	if mx, ok := num("maxclients"); ok && mx > 0 {
		obj.Metrics["connections_max"] = mx
		if c, ok := num("connected_clients"); ok {
			obj.Metrics["connections_usage_pct"] = c / mx * 100
		}
	}
	if hits, ok := num("keyspace_hits"); ok {
		if miss, _ := num("keyspace_misses"); hits+miss > 0 {
			obj.Metrics["cache_hit_pct"] = hits / (hits + miss) * 100
		}
	}
	keys := 0.0
	for k, v := range m {
		if strings.HasPrefix(k, "db") && strings.HasPrefix(v, "keys=") {
			if n, err := strconv.ParseFloat(strings.TrimPrefix(strings.SplitN(v, ",", 2)[0], "keys="), 64); err == nil {
				keys += n
			}
		}
	}
	obj.Metrics["redis_keys"] = keys
	set("replication_lag_seconds", "master_last_io_seconds_ago")
	obj.Labels["role"] = m["role"]
	if v := m["redis_version"]; v != "" {
		obj.Labels["version"] = v
	}
	if m["role"] == "slave" && m["master_link_status"] == "down" {
		obj.Status, obj.Output = agent.Critical, "replica: master link is down"
	}
	if m["rdb_last_bgsave_status"] == "err" && obj.Status == "" {
		obj.Status, obj.Output = agent.Warning, "last background save failed"
	}
}
