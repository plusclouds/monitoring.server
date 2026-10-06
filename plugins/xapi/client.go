// Package xapi holds the XCP-ng / XenServer plugins (F10): xapi.pool reads
// a pool through its master's API (JSON-RPC) and its hosts' performance
// data (rrd_updates), discovers hosts and VMs as child devices and reports
// hosts, VMs and storage repositories as objects.
package xapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// maxBody bounds an API response; a large pool's VM records are a few MB.
const maxBody = 64 << 20

// apiError is an error the API returned, such as SESSION_AUTHENTICATION_FAILED
// or HOST_IS_SLAVE (whose first parameter is the master's address).
type apiError struct {
	Code string
	Data []string
}

func (e *apiError) Error() string {
	if len(e.Data) == 0 {
		return "XAPI error " + e.Code
	}
	return "XAPI error " + e.Code + " " + strings.Join(e.Data, " ")
}

// client talks JSON-RPC 2.0 to one host of a pool.
type client struct {
	host    string // host or host:port
	http    *http.Client
	session string
	id      atomic.Int64
}

func newClient(t plugin.Target, host string, verify bool) *client {
	transport := &http.Transport{
		DialContext:         t.Network.DialContext(15*time.Second, nil),
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !verify}, //nolint:gosec // default: XCP-ng hosts ship self-signed certificates; verify_certificate turns checking on
		TLSHandshakeTimeout: 15 * time.Second,
		IdleConnTimeout:     30 * time.Second,
	}
	return &client{host: host, http: &http.Client{Transport: transport}}
}

func (c *client) close() { c.http.CloseIdleConnections() }

// call invokes method with params and decodes the result into out.
func (c *client) call(ctx context.Context, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": c.id.Add(1)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.host+"/jsonrpc", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", method, res.StatusCode)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string   `json:"message"`
			Data    []string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("%s: unreadable response: %w", method, err)
	}
	if resp.Error != nil {
		return &apiError{Code: resp.Error.Message, Data: resp.Error.Data}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, out)
}

// login opens a session. A pool member answers HOST_IS_SLAVE with the
// master's address; login follows it once.
func login(ctx context.Context, t plugin.Target, host string, verify bool) (*client, error) {
	cred, ok := t.Credentials["auth"]
	if !ok {
		return nil, errors.New(`no XAPI credential: assign an xapi credential (read-only RBAC role) in the "auth" role`)
	}
	user, pass := cred.Fields["username"], cred.Secret["password"].Reveal()
	for attempt := 0; ; attempt++ {
		c := newClient(t, host, verify)
		var session string
		err := c.call(ctx, "session.login_with_password", &session, user, pass, "1.0", "monitor")
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "HOST_IS_SLAVE" && len(ae.Data) > 0 && attempt == 0 {
			c.close()
			host = withPort(ae.Data[0], host)
			continue
		}
		if err != nil {
			c.close()
			return nil, err
		}
		c.session = session
		return c, nil
	}
}

// withPort keeps the port of the address we used when following a master
// redirect (the API gives the master's IP only).
func withPort(master, used string) string {
	if strings.Contains(master, ":") && !strings.HasPrefix(master, "[") {
		return master // an IPv6 address or already with a port
	}
	if i := strings.LastIndex(used, ":"); i > 0 && !strings.Contains(used[:i], ":") {
		return master + used[i:]
	}
	return master
}

func (c *client) logout() {
	if c.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.call(ctx, "session.logout", nil, c.session)
	c.close()
}

// records reads class.get_all_records into out (a map of ref to record).
func (c *client) records(ctx context.Context, class string, out any) error {
	return c.call(ctx, class+".get_all_records", out, c.session)
}

// rrd reads the latest performance data of one host and the VMs on it.
func (c *client) rrd(ctx context.Context, t plugin.Target, host string, verify bool, since time.Time) (rrdUpdate, error) {
	q := url.Values{"session_id": {c.session}, "start": {strconv.FormatInt(since.Unix(), 10)}, "cf": {"AVERAGE"},
		"host": {"true"}, "json": {"true"}, "interval": {"60"}}
	hc := newClient(t, host, verify)
	defer hc.close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/rrd_updates?"+q.Encode(), nil)
	if err != nil {
		return rrdUpdate{}, err
	}
	res, err := hc.http.Do(req)
	if err != nil {
		return rrdUpdate{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return rrdUpdate{}, fmt.Errorf("rrd_updates from %s: HTTP %d", host, res.StatusCode)
	}
	var u rrdUpdate
	return u, json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(&u)
}

// rrdUpdate is the JSON form of rrd_updates: rows of string values, one
// column per legend entry such as "AVERAGE:vm:<uuid>:cpu0".
type rrdUpdate struct {
	Meta struct {
		Legend []string `json:"legend"`
		Data   []struct {
			T      xint     `json:"t"`
			Values []string `json:"values"`
		} `json:"data"`
	} `json:"meta"`
}

// latest returns, per object ("host:<uuid>", "vm:<uuid>") and data source,
// the most recent value that is a number.
func (u rrdUpdate) latest() map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	var newest int64 = -1
	row := -1
	for i, d := range u.Meta.Data {
		if int64(d.T) > newest {
			newest, row = int64(d.T), i
		}
	}
	for col, name := range u.Meta.Legend {
		parts := strings.SplitN(name, ":", 4) // AVERAGE:vm:<uuid>:<source>
		if len(parts) != 4 {
			continue
		}
		// The newest row first; an older one when the newest is NaN.
		for _, r := range rowsFrom(u, row) {
			vals := u.Meta.Data[r].Values
			if col >= len(vals) {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(vals[col]), 64)
			if err != nil || math.IsNaN(v) {
				continue
			}
			key := parts[1] + ":" + parts[2]
			if out[key] == nil {
				out[key] = map[string]float64{}
			}
			out[key][parts[3]] = v
			break
		}
	}
	return out
}

func rowsFrom(u rrdUpdate, newest int) []int {
	if newest < 0 {
		return nil
	}
	type rt struct {
		i int
		t int64
	}
	var rows []rt
	for i, d := range u.Meta.Data {
		rows = append(rows, rt{i, int64(d.T)})
	}
	// Newest first.
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].t > rows[j-1].t; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = r.i
	}
	return out
}

// xint is an API int: a JSON number or, as XAPI sometimes sends it, a
// string.
type xint int64

func (x *xint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*x = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*x = xint(f)
	return nil
}
