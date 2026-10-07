// Package mqtt is the embedded MQTT broker of the ingest role (F08, F12,
// ADR-0013). Devices only publish; the broker authenticates them by
// credential (which decides the tenant), allows topics under their own key,
// turns each message into results for the engine, and reports connects and
// disconnects through an mqtt.connection check per device. Nothing is
// routed to subscribers or retained.
package mqtt

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/plusclouds/monitoring.server/internal/auth"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/tlsconf"
)

// Listener IDs: devices on the plain one need a credential with allow_plain.
const (
	listenerTLS   = "mqtts"
	listenerPlain = "mqtt-plain"
)

// Options configure the broker.
type Options struct {
	Config   config.IngestMQTT
	TLS      config.TLSPolicy
	NodeID   string
	System   *pgxpool.Pool // BYPASSRLS: credentials of every tenant
	Results  chan<- runner.Result
	Logger   *slog.Logger
	Registry prometheus.Registerer
	// TLSListen and PlainListen override the config's addresses (tests);
	// PlainListen "" with Config.Legacy.Enabled false means no plain listener.
	TLSListen, PlainListen string
	// NoTLS runs the "TLS" listener without TLS (tests only).
	NoTLS bool
}

// Broker is the embedded MQTT server.
type Broker struct {
	o   Options
	log *slog.Logger
	srv *mochi.Server
	ctx context.Context

	clients sync.Map // *mochi.Client -> *client
	conns   atomic.Int64

	authSem  chan struct{}
	credMu   sync.Mutex
	creds    map[string]cachedCred // username
	verified map[[32]byte]time.Time
	ipMu     sync.Mutex
	ipLimits map[string]*ipLimiter

	devMu   sync.Mutex
	devices map[devKey]*device

	touchMu sync.Mutex
	touched map[uuid.UUID]time.Time // data check -> newest message time

	connections prometheus.Gauge
	authFails   *prometheus.CounterVec
	messages    *prometheus.CounterVec
}

type cachedCred struct {
	cred    *credential // nil: unknown username
	fetched time.Time
}

type credential struct {
	id, tenant   uuid.UUID
	kind         string
	deviceKey    string
	profile      string
	allowPlain   bool
	autoRegister bool
	hash         string
}

type ipLimiter struct {
	*rate.Limiter
	seen time.Time
}

// client is the broker's state of one connection.
type client struct {
	cred        *credential
	limiter     *rate.Limiter
	remote      string
	keepalive   int
	connectedAt time.Time

	mu      sync.Mutex
	key     string  // the first device key it published under
	dev     *device // that key's device
	gateway bool    // publishes for several keys: no connection events
	sentOK  bool    // the connection check heard data in this session
}

// New builds the broker and registers its metrics.
func New(o Options) (*Broker, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	b := &Broker{
		o: o, log: o.Logger,
		authSem:  make(chan struct{}, max(o.Config.MaxConcurrentAuth, 1)),
		creds:    map[string]cachedCred{},
		verified: map[[32]byte]time.Time{},
		ipLimits: map[string]*ipLimiter{},
		devices:  map[devKey]*device{},
		touched:  map[uuid.UUID]time.Time{},
		connections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mqtt_connections", Help: "Open MQTT connections on this node."}),
		authFails: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mqtt_auth_failures_total", Help: "Refused MQTT connections and topics by reason."}, []string{"reason"}),
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mqtt_messages_total", Help: "MQTT messages by outcome."}, []string{"outcome"}),
	}
	if o.Registry != nil {
		for _, c := range []prometheus.Collector{b.connections, b.authFails, b.messages} {
			if err := o.Registry.Register(c); err != nil {
				return nil, err
			}
		}
	}
	caps := mochi.NewDefaultServerCapabilities()
	if o.Config.MaxConnections > 0 {
		caps.MaximumClients = int64(o.Config.MaxConnections)
	}
	caps.MaximumPacketSize = uint32(min(max(int64(o.Config.MaxPacketSize), 1024), 1<<28)) //nolint:gosec // bounded
	caps.MaximumSessionExpiryInterval = 0                                                 // clean sessions only, nothing kept
	caps.RetainAvailable = 0
	caps.MaximumQos = 1
	caps.WildcardSubAvailable = 0
	caps.SharedSubAvailable = 0
	b.srv = mochi.New(&mochi.Options{Capabilities: caps, InlineClient: false,
		Logger: slog.New(warnOnly{o.Logger.Handler()})})
	if err := b.srv.AddHook(&hook{b: b}, nil); err != nil {
		return nil, err
	}
	return b, nil
}

// Run serves until ctx ends.
func (b *Broker) Run(ctx context.Context) error {
	b.ctx = ctx
	tlsAddr := b.o.Config.Listen
	if b.o.TLSListen != "" {
		tlsAddr = b.o.TLSListen
	}
	lc := listeners.Config{ID: listenerTLS, Address: tlsAddr}
	if !b.o.NoTLS {
		tc, err := tlsconf.ServerReloading(b.o.TLS, b.o.Config.TLS)
		if err != nil {
			return fmt.Errorf("ingest.mqtt.tls: %w", err)
		}
		lc.TLSConfig = tc
	}
	if err := b.srv.AddListener(listeners.NewTCP(lc)); err != nil {
		return err
	}
	plain := b.o.PlainListen
	if plain == "" && b.o.Config.Legacy.Enabled {
		plain = b.o.Config.Legacy.Listen
	}
	if plain != "" {
		if err := b.srv.AddListener(listeners.NewTCP(listeners.Config{ID: listenerPlain, Address: plain})); err != nil {
			return err
		}
	}
	if b.o.Config.ProxyProtocol {
		b.log.Warn("ingest.mqtt.proxy_protocol is not implemented yet; client addresses are the load balancer's")
	}
	if err := b.srv.Serve(); err != nil {
		return err
	}
	b.log.Info("mqtt listening", "tls", tlsAddr, "plain", plain)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			err := b.srv.Close()
			b.flushTouched(context.Background())
			return err
		case <-tick.C:
			b.flushTouched(ctx)
			b.expireCaches()
		}
	}
}

// hook connects mochi's events to the broker.
type hook struct {
	mochi.HookBase
	b *Broker
}

func (h *hook) ID() string { return "monitor" }

func (h *hook) Provides(x byte) bool {
	switch x {
	case mochi.OnConnectAuthenticate, mochi.OnACLCheck, mochi.OnDisconnect, mochi.OnPublish:
		return true
	}
	return false
}

func (h *hook) OnConnectAuthenticate(cl *mochi.Client, pk packets.Packet) bool {
	return h.b.authenticate(cl, pk)
}

func (h *hook) OnACLCheck(cl *mochi.Client, topic string, write bool) bool {
	return h.b.allowed(cl, topic, write)
}

func (h *hook) OnPublish(cl *mochi.Client, pk packets.Packet) (packets.Packet, error) {
	h.b.publish(cl, pk.TopicName, pk.Payload)
	// Acknowledged, never routed or retained: there are no subscribers.
	pk.Ignore = true
	pk.FixedHeader.Retain = false
	return pk, nil
}

func (h *hook) OnDisconnect(cl *mochi.Client, err error, _ bool) {
	h.b.disconnected(cl, err)
}

// authenticate checks the username and password, the listener and the
// connection limits.
func (b *Broker) authenticate(cl *mochi.Client, pk packets.Packet) bool {
	remote := cl.Net.Remote
	if !b.allowIP(remote) {
		b.authFails.WithLabelValues("connect-rate").Inc()
		return false
	}
	username := string(cl.Properties.Username)
	password := string(pk.Connect.Password)
	if username == "" || password == "" {
		b.authFails.WithLabelValues("no-credentials").Inc()
		return false
	}
	cred, err := b.credential(username)
	if err != nil {
		b.log.Error("mqtt credential lookup", "error", err)
		b.authFails.WithLabelValues("error").Inc()
		return false
	}
	if cred == nil || !b.verify(username, password, cred.hash) {
		b.authFails.WithLabelValues("bad-credentials").Inc()
		return false
	}
	if cl.Net.Listener == listenerPlain && !cred.allowPlain {
		b.authFails.WithLabelValues("plain-not-allowed").Inc()
		return false
	}
	c := &client{cred: cred, remote: remote, keepalive: int(pk.Connect.Keepalive),
		connectedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if pr := b.o.Config.PublishRate; pr.PerSecond > 0 {
		c.limiter = rate.NewLimiter(rate.Limit(pr.PerSecond), max(pr.Burst, 1))
	}
	b.clients.Store(cl, c)
	b.connections.Set(float64(b.conns.Add(1)))
	go b.touchCredential(cred.id)
	return true
}

// credential returns a username's credential, cached; nil when unknown,
// disabled or of a tenant that is not active.
func (b *Broker) credential(username string) (*credential, error) {
	ttl := b.o.Config.CredentialCacheTTL.D()
	b.credMu.Lock()
	if c, ok := b.creds[username]; ok && time.Since(c.fetched) < ttl {
		b.credMu.Unlock()
		return c.cred, nil
	}
	b.credMu.Unlock()
	ctx, cancel := context.WithTimeout(b.base(), 5*time.Second)
	defer cancel()
	var c credential
	var key *string
	err := b.o.System.QueryRow(ctx, `
		SELECT c.id, c.tenant_id, c.kind, c.device_key, c.profile, c.allow_plain, c.auto_register, c.password_hash
		  FROM mqtt_credentials c JOIN tenants t ON t.id = c.tenant_id
		 WHERE c.username = $1 AND c.enabled AND t.status = 'active'`, username).
		Scan(&c.id, &c.tenant, &c.kind, &key, &c.profile, &c.allowPlain, &c.autoRegister, &c.hash)
	var out *credential
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		if key != nil {
			c.deviceKey = *key
		}
		out = &c
	}
	b.credMu.Lock()
	b.creds[username] = cachedCred{cred: out, fetched: time.Now()}
	b.credMu.Unlock()
	return out, nil
}

// verify checks a password with Argon2id; a recent success for the same
// username, password and hash is remembered so reconnect storms stay cheap.
func (b *Broker) verify(username, password, hash string) bool {
	k := sha256.Sum256([]byte(username + "\x00" + password + "\x00" + hash))
	ttl := b.o.Config.CredentialCacheTTL.D()
	b.credMu.Lock()
	if t, ok := b.verified[k]; ok && time.Since(t) < ttl {
		b.credMu.Unlock()
		return true
	}
	b.credMu.Unlock()
	b.authSem <- struct{}{}
	ok, err := auth.VerifyPassword(password, hash)
	<-b.authSem
	if err != nil || !ok {
		return false
	}
	b.credMu.Lock()
	b.verified[k] = time.Now()
	b.credMu.Unlock()
	return true
}

func (b *Broker) touchCredential(id uuid.UUID) {
	ctx, cancel := context.WithTimeout(b.base(), 5*time.Second)
	defer cancel()
	_, _ = b.o.System.Exec(ctx, `UPDATE mqtt_credentials SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id)
}

func (b *Broker) allowIP(remote string) bool {
	r := b.o.Config.ConnectRatePerIP
	if r.PerSecond <= 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	b.ipMu.Lock()
	defer b.ipMu.Unlock()
	l := b.ipLimits[host]
	if l == nil {
		l = &ipLimiter{Limiter: rate.NewLimiter(rate.Limit(r.PerSecond), max(r.Burst, 1))}
		b.ipLimits[host] = l
	}
	l.seen = time.Now()
	return l.Allow()
}

// allowed is the topic ACL: publish only, under <prefix>/<key>/..., a
// device credential only under its own key. Nothing may subscribe.
func (b *Broker) allowed(cl *mochi.Client, topic string, write bool) bool {
	v, ok := b.clients.Load(cl)
	if !ok {
		return false
	}
	c := v.(*client)
	if !write {
		b.authFails.WithLabelValues("subscribe-denied").Inc()
		return false
	}
	_, key, ok := splitTopic(topic)
	if !ok || (c.cred.kind == "device" && !strings.EqualFold(key, c.cred.deviceKey)) {
		b.authFails.WithLabelValues("topic-denied").Inc()
		return false
	}
	return true
}

// splitTopic reads <prefix>/<key>[/...].
func splitTopic(topic string) (prefix, key string, ok bool) {
	parts := strings.SplitN(topic, "/", 3)
	if len(parts) < 2 || parts[0] == "" || !inventory.DeviceKeyRe.MatchString(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (b *Broker) base() context.Context {
	if b.ctx != nil {
		return b.ctx
	}
	return context.Background()
}

func (b *Broker) expireCaches() {
	ttl := b.o.Config.CredentialCacheTTL.D()
	now := time.Now()
	b.credMu.Lock()
	for k, c := range b.creds {
		if now.Sub(c.fetched) > ttl {
			delete(b.creds, k)
		}
	}
	for k, t := range b.verified {
		if now.Sub(t) > ttl {
			delete(b.verified, k)
		}
	}
	b.credMu.Unlock()
	b.ipMu.Lock()
	for k, l := range b.ipLimits {
		if now.Sub(l.seen) > 10*time.Minute {
			delete(b.ipLimits, k)
		}
	}
	b.ipMu.Unlock()
	b.devMu.Lock()
	for k, d := range b.devices {
		if now.Sub(d.loaded) > deviceTTL {
			delete(b.devices, k)
		}
	}
	b.devMu.Unlock()
}

// warnOnly passes mochi's warnings and errors, not its per-connection info.
type warnOnly struct{ slog.Handler }

func (w warnOnly) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && w.Handler.Enabled(ctx, l)
}

func (w warnOnly) WithAttrs(a []slog.Attr) slog.Handler { return warnOnly{w.Handler.WithAttrs(a)} }
func (w warnOnly) WithGroup(n string) slog.Handler      { return warnOnly{w.Handler.WithGroup(n)} }
