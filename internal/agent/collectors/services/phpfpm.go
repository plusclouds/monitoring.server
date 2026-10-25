package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

type phpfpm struct {
	name, network, addr, path string
	timeout                   time.Duration
	conns                     rate
	now                       func() time.Time
}

func newPHPFPM(o agent.Options) (agent.Collector, error) {
	to, err := o.Duration("timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	addr := o.String("addr", "127.0.0.1:9000")
	network := "tcp"
	if s, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, addr = "unix", s
	} else if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("addr %q must be host:port or unix:/path", addr)
	}
	return &phpfpm{name: o.Name, network: network, addr: addr, path: o.String("status_path", "/status"), timeout: to, now: time.Now}, nil
}

// Collect asks php-fpm for its status page over FastCGI, the way nginx does,
// so the status path need not be reachable over HTTP. Set
// pm.status_path = /status in the pool.
func (p *phpfpm) Collect(ctx context.Context) ([]agent.Object, error) {
	obj := object("phpfpm", p.name, map[string]string{"addr": p.addr})
	start := time.Now()
	body, err := fcgiGet(ctx, p.network, p.addr, p.path, "json", p.timeout)
	if err != nil {
		return down(obj, err), nil
	}
	var st struct {
		Pool       string  `json:"pool"`
		Since      float64 `json:"start since"`
		Accepted   float64 `json:"accepted conn"`
		Queue      float64 `json:"listen queue"`
		Idle       float64 `json:"idle processes"`
		Active     float64 `json:"active processes"`
		Total      float64 `json:"total processes"`
		MaxReached float64 `json:"max children reached"`
		Slow       float64 `json:"slow requests"`
	}
	if err := json.Unmarshal(body, &st); err != nil || st.Pool == "" {
		return down(obj, fmt.Errorf("not a php-fpm status page (is pm.status_path = %s set?)", p.path)), nil
	}
	m := obj.Metrics
	m["up"], m["response_ms"] = 1, ms(time.Since(start))
	m["phpfpm_active"], m["phpfpm_idle"], m["phpfpm_queue"] = st.Active, st.Idle, st.Queue
	m["phpfpm_max_children_reached"], m["slow_queries"], m["uptime_seconds"] = st.MaxReached, st.Slow, st.Since
	m["connections"] = st.Total
	if r, ok := p.conns.next(p.now(), st.Accepted); ok {
		m["ops_per_sec"] = r
	}
	obj.Labels["pool"] = st.Pool
	return []agent.Object{obj}, nil
}

// FastCGI record types.
const (
	fcgiBegin  = 1
	fcgiEnd    = 3
	fcgiParams = 4
	fcgiStdin  = 5
	fcgiStdout = 6
	fcgiStderr = 7
)

func fcgiRecord(buf *bytes.Buffer, typ byte, data []byte) {
	_ = binary.Write(buf, binary.BigEndian, struct {
		Version, Type byte
		ID, Len       uint16
		Pad, Reserved byte
	}{1, typ, 1, uint16(len(data)), 0, 0}) //nolint:gosec // records here carry at most a few hundred bytes
	buf.Write(data)
}

func fcgiPair(buf *bytes.Buffer, k, v string) {
	for _, n := range []int{len(k), len(v)} {
		if n < 128 {
			buf.WriteByte(byte(n)) //nolint:gosec // n < 128
		} else {
			_ = binary.Write(buf, binary.BigEndian, uint32(n)|1<<31) //nolint:gosec // n is a short name or value
		}
	}
	buf.WriteString(k)
	buf.WriteString(v)
}

// fcgiGet performs one GET request and returns the response body.
func fcgiGet(ctx context.Context, network, addr, path, query string, timeout time.Duration) ([]byte, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	var req, params bytes.Buffer
	fcgiRecord(&req, fcgiBegin, []byte{0, 1, 0, 0, 0, 0, 0, 0}) // responder, close after
	for _, kv := range [][2]string{{"SCRIPT_NAME", path}, {"SCRIPT_FILENAME", path}, {"REQUEST_URI", path + "?" + query},
		{"QUERY_STRING", query}, {"REQUEST_METHOD", "GET"}, {"SERVER_PROTOCOL", "HTTP/1.1"}, {"GATEWAY_INTERFACE", "CGI/1.1"}} {
		fcgiPair(&params, kv[0], kv[1])
	}
	fcgiRecord(&req, fcgiParams, params.Bytes())
	fcgiRecord(&req, fcgiParams, nil)
	fcgiRecord(&req, fcgiStdin, nil)
	if _, err := conn.Write(req.Bytes()); err != nil {
		return nil, err
	}

	var out, errOut bytes.Buffer
	for {
		var h struct {
			Version, Type byte
			ID, Len       uint16
			Pad, Reserved byte
		}
		if err := binary.Read(conn, binary.BigEndian, &h); err != nil {
			return nil, err
		}
		data := make([]byte, int(h.Len)+int(h.Pad))
		if _, err := io.ReadFull(conn, data); err != nil {
			return nil, err
		}
		data = data[:h.Len]
		switch h.Type {
		case fcgiStdout:
			if out.Len()+len(data) > 1<<20 {
				return nil, errors.New("response too large")
			}
			out.Write(data)
		case fcgiStderr:
			errOut.Write(data)
		case fcgiEnd:
			head, body, ok := bytes.Cut(out.Bytes(), []byte("\r\n\r\n"))
			if !ok {
				return nil, errors.New("malformed FastCGI response")
			}
			if bytes.HasPrefix(head, []byte("Status: ")) && !bytes.HasPrefix(head, []byte("Status: 200")) {
				line, _, _ := bytes.Cut(head, []byte("\r\n"))
				return nil, fmt.Errorf("php-fpm answered %s", bytes.TrimPrefix(line, []byte("Status: ")))
			}
			return body, nil
		}
	}
}
