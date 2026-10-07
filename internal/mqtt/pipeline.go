package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/ingest"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/plugins/push"
)

// deviceTTL is how long a device's checks and config are cached; changes
// through the API apply within it.
const deviceTTL = 30 * time.Second

type devKey struct {
	tenant uuid.UUID
	key    string
}

// device is an MQTT device as the pipeline needs it.
type device struct {
	id, dataCheck uuid.UUID
	connCheck     *uuid.UUID
	config        json.RawMessage
	interval      int
	enabled       bool
	inventory     map[string]any
	loaded        time.Time
}

var mqttPlugin = &push.MQTT{}

// publish handles one message: find (or register) the device, learn new
// fields, send the results to the engine and tell the connection check
// that data flows.
func (b *Broker) publish(cl *mochi.Client, topic string, payload []byte) {
	v, ok := b.clients.Load(cl)
	if !ok {
		return
	}
	c := v.(*client)
	if c.limiter != nil && !c.limiter.Allow() {
		b.messages.WithLabelValues("rate-limited").Inc()
		return
	}
	prefix, rawKey, ok := splitTopic(topic)
	if !ok {
		b.messages.WithLabelValues("bad-topic").Inc()
		return
	}
	key := inventory.NormalizeDeviceKey(rawKey)
	ctx, cancel := context.WithTimeout(b.base(), 10*time.Second)
	defer cancel()
	dev, err := b.device(ctx, c, key, prefix)
	if err != nil {
		b.log.Error("mqtt device", "device_key", key, "error", err)
		b.messages.WithLabelValues("error").Inc()
		return
	}
	if dev == nil { // dropped: auto-registration off or device limit
		b.messages.WithLabelValues("unregistered").Inc()
		return
	}
	b.bindClient(ctx, cl, c, key, dev)
	if !dev.enabled {
		b.messages.WithLabelValues("check-disabled").Inc()
		return
	}
	now := time.Now().UTC()
	cfg, err := b.discover(ctx, key, c.cred.tenant, dev, payload)
	if err != nil {
		b.log.Error("mqtt field discovery", "device_key", key, "error", err)
	}
	results, err := mqttPlugin.Parse(cfg, payload)
	if err != nil {
		b.messages.WithLabelValues("invalid").Inc()
		b.log.Debug("mqtt payload", "device_key", key, "error", err)
		return
	}
	layout, _ := mqttPlugin.MetricsFor(cfg)
	interval := time.Duration(dev.interval) * time.Second
	var newest time.Time
	for _, r := range results {
		r.Time = ingest.Window(&r, now)
		if r.Time.After(newest) {
			newest = r.Time
		}
		if !b.send(runner.Result{TenantID: c.cred.tenant, DeviceID: dev.id, CheckID: dev.dataCheck, Plugin: "push.mqtt",
			Interval: interval, Scheduled: r.Time, Result: r, Layout: layout}) {
			return
		}
	}
	b.touch(dev.dataCheck, newest)
	b.messages.WithLabelValues("accepted").Inc()
	b.statusInventory(ctx, dev, cfg, payload, prefix)
	b.dataFlows(c, dev, now)
}

// send hands a result to the engine; a full channel for 5 s drops it.
func (b *Broker) send(r runner.Result) bool {
	select {
	case b.o.Results <- r:
		return true
	case <-b.base().Done():
		return false
	case <-time.After(5 * time.Second):
		b.messages.WithLabelValues("engine-busy").Inc()
		return false
	}
}

// device finds the device of a key, registering it when the credential
// allows. nil means the message is dropped (and counted as unregistered).
func (b *Broker) device(ctx context.Context, c *client, key, vendor string) (*device, error) {
	k := devKey{c.cred.tenant, key}
	b.devMu.Lock()
	d, ok := b.devices[k]
	b.devMu.Unlock()
	if ok && time.Since(d.loaded) < deviceTTL {
		return d, nil
	}
	d, err := b.loadDevice(ctx, k)
	if err != nil || d != nil {
		if d != nil {
			b.cacheDevice(k, d)
		}
		return d, err
	}
	reason := ""
	err = pgx.BeginFunc(ctx, b.o.System, func(tx pgx.Tx) error {
		if !c.cred.autoRegister {
			reason = "auto-registration is off for the credential"
			return inventory.RecordUnregistered(ctx, tx, k.tenant, key, c.cred.id, reason)
		}
		var maxDevices int
		if err := tx.QueryRow(ctx, `SELECT max_devices FROM tenants WHERE id = $1`, k.tenant).Scan(&maxDevices); err != nil {
			return err
		}
		_, err := inventory.AutoRegisterMQTT(ctx, tx, k.tenant, maxDevices, key, c.cred.profile, vendor, c.keepalive)
		return err
	})
	var conflict *errs.Conflict
	switch {
	case errors.As(err, &conflict) && conflict.Type == "limit-reached":
		reason = "the tenant is at its device limit"
		err = pgx.BeginFunc(ctx, b.o.System, func(tx pgx.Tx) error {
			return inventory.RecordUnregistered(ctx, tx, k.tenant, key, c.cred.id, reason)
		})
		return nil, err
	case errors.As(err, &conflict):
		// Another connection registered the key at the same moment.
		b.log.Debug("mqtt registration raced", "device_key", key, "error", err)
	case err != nil:
		return nil, err
	}
	if reason != "" {
		return nil, nil
	}
	d, err = b.loadDevice(ctx, k)
	if d != nil {
		b.cacheDevice(k, d)
		b.log.Info("mqtt device registered", "tenant_id", k.tenant, "device_key", key, "device_id", d.id)
	}
	return d, err
}

func (b *Broker) cacheDevice(k devKey, d *device) {
	b.devMu.Lock()
	b.devices[k] = d
	b.devMu.Unlock()
}

func (b *Broker) loadDevice(ctx context.Context, k devKey) (*device, error) {
	d := &device{loaded: time.Now()}
	err := b.o.System.QueryRow(ctx, `
		SELECT md.device_id, md.data_check_id, md.connection_check_id, c.config, c.interval_seconds,
		       c.enabled AND t.status = 'active', dv.inventory
		  FROM mqtt_devices md
		  JOIN checks c ON c.id = md.data_check_id
		  JOIN devices dv ON dv.id = md.device_id
		  JOIN tenants t ON t.id = md.tenant_id
		 WHERE md.tenant_id = $1 AND md.device_key = $2`, k.tenant, k.key).
		Scan(&d.id, &d.dataCheck, &d.connCheck, &d.config, &d.interval, &d.enabled, &d.inventory)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// discover adds keys the device sends for the first time to its check's
// fields (up to max_discovered_fields), so they are stored from now on.
func (b *Broker) discover(ctx context.Context, key string, tenant uuid.UUID, d *device, payload []byte) (json.RawMessage, error) {
	cfg := d.config
	if !push.Discoverable(cfg) {
		return cfg, nil
	}
	var c map[string]any
	if err := json.Unmarshal(cfg, &c); err != nil {
		return cfg, err
	}
	profile, _ := c["profile"].(string)
	msg, err := push.Decode(profile, payload)
	if err != nil {
		return cfg, nil // Parse reports it
	}
	layout, _ := mqttPlugin.MetricsFor(cfg)
	known := map[string]bool{}
	for _, d := range layout {
		known[d.Name] = true
	}
	var fields []string
	if f, ok := c["fields"].([]any); ok {
		for _, x := range f {
			if s, ok := x.(string); ok {
				fields = append(fields, s)
			}
		}
	}
	limit := b.o.Config.MaxDiscoveredFields
	if limit <= 0 {
		limit = 200
	}
	added := false
	for _, name := range slices.Sorted(maps.Keys(msg.Values)) {
		if known[name] {
			continue
		}
		if len(fields) >= limit {
			b.messages.WithLabelValues("fields-capped").Inc()
			break
		}
		fields = append(fields, name)
		known[name], added = true, true
	}
	if !added {
		return cfg, nil
	}
	slices.Sort(fields)
	c["fields"] = fields
	next, err := json.Marshal(c)
	if err != nil {
		return cfg, err
	}
	if _, err := b.o.System.Exec(ctx, `UPDATE checks SET config = $2, updated_at = now() WHERE id = $1`, d.dataCheck, next); err != nil {
		return cfg, err
	}
	b.devMu.Lock()
	d.config = next
	b.devMu.Unlock()
	b.log.Info("mqtt fields discovered", "tenant_id", tenant, "device_key", key, "fields", len(fields))
	return next, nil
}

// statusInventory stores a FixLean status payload's firmware, product and
// addresses in the device's inventory when they changed.
func (b *Broker) statusInventory(ctx context.Context, d *device, cfg json.RawMessage, payload []byte, vendor string) {
	var c struct {
		Profile string `json:"profile"`
	}
	if json.Unmarshal(cfg, &c) != nil || c.Profile != push.ProfileFixLean {
		return
	}
	msg, err := push.Decode(c.Profile, payload)
	if err != nil || !msg.Status {
		return
	}
	info := map[string]any{"vendor": vendor}
	for k, v := range msg.Info {
		info[k] = v
	}
	same := true
	for k, v := range info {
		if d.inventory[k] != v {
			same = false
		}
	}
	if same {
		return
	}
	if _, err := b.o.System.Exec(ctx, `UPDATE devices SET inventory = inventory || $2, updated_at = now() WHERE id = $1`,
		d.id, info); err != nil {
		b.log.Error("mqtt inventory", "device_id", d.id, "error", err)
		return
	}
	b.devMu.Lock()
	if d.inventory == nil {
		d.inventory = map[string]any{}
	}
	maps.Copy(d.inventory, info)
	b.devMu.Unlock()
}

// bindClient ties a connection to the device of the first key it publishes
// under and records the session. A client that publishes for a second key
// is a gateway: its connection says nothing about one device.
func (b *Broker) bindClient(ctx context.Context, cl *mochi.Client, c *client, key string, d *device) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.gateway:
		return
	case c.key == "":
		c.key, c.dev = key, d
		if _, err := b.o.System.Exec(ctx, `
			INSERT INTO mqtt_sessions (device_id, tenant_id, node_id, client_id, remote, keepalive, connected_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (device_id) DO UPDATE SET node_id = excluded.node_id, client_id = excluded.client_id,
			       remote = excluded.remote, keepalive = excluded.keepalive, connected_at = excluded.connected_at`,
			d.id, c.cred.tenant, b.o.NodeID, cl.ID, c.remote, c.keepalive, c.connectedAt); err != nil {
			b.log.Error("mqtt session", "device_id", d.id, "error", err)
		}
	case c.key != key:
		c.gateway = true
	}
}

// dataFlows reports the connection OK with the first message of a session
// (not on connect: "recovered" means data flows again, F12).
func (b *Broker) dataFlows(c *client, d *device, now time.Time) {
	c.mu.Lock()
	if c.gateway || c.sentOK || c.dev != d || d.connCheck == nil {
		c.mu.Unlock()
		return
	}
	c.sentOK = true
	c.mu.Unlock()
	b.send(runner.Result{TenantID: c.cred.tenant, DeviceID: d.id, CheckID: *d.connCheck, Plugin: "mqtt.connection",
		Interval: time.Duration(d.interval) * time.Second, Scheduled: now,
		Result: plugin.Result{Status: plugin.OK, Time: now,
			Output: fmt.Sprintf("connected from %s since %s, keepalive %d s", c.remote, c.connectedAt.Format(time.RFC3339), c.keepalive)}})
}

// disconnected reports CRITICAL on the device's connection check, unless
// the node is shutting down, a newer session took over, or the session
// row belongs to another connection by now.
func (b *Broker) disconnected(cl *mochi.Client, err error) {
	v, ok := b.clients.LoadAndDelete(cl)
	if !ok {
		return
	}
	b.connections.Set(float64(b.conns.Add(-1)))
	c := v.(*client)
	c.mu.Lock()
	d, gateway := c.dev, c.gateway
	c.mu.Unlock()
	if d == nil || gateway {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, dbErr := b.o.System.Exec(ctx, `DELETE FROM mqtt_sessions
		WHERE device_id = $1 AND node_id = $2 AND client_id = $3 AND connected_at = $4`, d.id, b.o.NodeID, cl.ID, c.connectedAt)
	if dbErr != nil {
		b.log.Error("mqtt session end", "device_id", d.id, "error", dbErr)
		return
	}
	if tag.RowsAffected() == 0 || d.connCheck == nil ||
		errors.Is(err, packets.ErrServerShuttingDown) || errors.Is(err, packets.ErrSessionTakenOver) {
		return
	}
	reason := "disconnected"
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		reason = fmt.Sprintf("keepalive timeout (no packet for 1.5 × %d s)", c.keepalive)
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, packets.CodeDisconnect):
	default:
		reason = "disconnected: " + err.Error()
	}
	now := time.Now().UTC()
	select {
	case b.o.Results <- runner.Result{TenantID: c.cred.tenant, DeviceID: d.id, CheckID: *d.connCheck, Plugin: "mqtt.connection",
		Interval: time.Duration(d.interval) * time.Second, Scheduled: now,
		Result: plugin.Result{Status: plugin.Critical, Time: now, Output: reason}}:
	case <-time.After(5 * time.Second):
		b.messages.WithLabelValues("engine-busy").Inc()
	}
}

// touch remembers a data check's newest message time for the last-seen
// rule; flushTouched writes them in one statement every 2 s.
func (b *Broker) touch(check uuid.UUID, t time.Time) {
	b.touchMu.Lock()
	if cur, ok := b.touched[check]; !ok || t.After(cur) {
		b.touched[check] = t
	}
	b.touchMu.Unlock()
}

func (b *Broker) flushTouched(ctx context.Context) {
	b.touchMu.Lock()
	if len(b.touched) == 0 {
		b.touchMu.Unlock()
		return
	}
	ids := make([]uuid.UUID, 0, len(b.touched))
	times := make([]time.Time, 0, len(b.touched))
	for id, t := range b.touched {
		ids = append(ids, id)
		times = append(times, t)
	}
	b.touched = map[uuid.UUID]time.Time{}
	b.touchMu.Unlock()
	if _, err := b.o.System.Exec(ctx, `
		UPDATE push_sources p SET last_push_at = greatest(p.last_push_at, v.t), stale_reported_at = NULL
		  FROM unnest($1::uuid[], $2::timestamptz[]) AS v (id, t) WHERE p.check_id = v.id`, ids, times); err != nil {
		b.log.Error("mqtt last seen", "error", err)
	}
}

// Flush writes pending last-seen times now (tests).
func (b *Broker) Flush(ctx context.Context) { b.flushTouched(ctx) }
