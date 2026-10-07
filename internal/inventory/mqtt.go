package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/auth"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/plugins/push"
)

// MQTT credentials and devices (F08, F12, ADR-0013).

// MQTTCredential lets devices connect to the embedded broker. The
// credential decides the tenant; a device credential may only publish
// under its own key, a shared one under any key of the tenant.
type MQTTCredential struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	Name         string
	Username     string
	Kind         string  // "device" or "shared"
	DeviceKey    *string // device kind only
	Profile      string  // push.ProfileFixLean or push.ProfileJSON
	AllowPlain   bool
	AutoRegister bool
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastUsedAt   *time.Time
	Password     string // only right after create or rotate
}

// MQTTCredentialInput is the writable part of a credential.
type MQTTCredentialInput struct {
	Name         string
	Username     string // empty: generated
	Password     string // empty: generated
	Kind         string
	DeviceKey    *string
	Profile      string
	AllowPlain   bool
	AutoRegister *bool // nil: true
	Enabled      *bool // nil: true
}

//nolint:gosec // column names, not credentials
const mqttCredCols = `id, tenant_id, name, username, kind, device_key, profile, allow_plain, auto_register, enabled,
	created_at, updated_at, last_used_at`

func scanMQTTCredential(row pgx.Row) (MQTTCredential, error) {
	var c MQTTCredential
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Username, &c.Kind, &c.DeviceKey, &c.Profile, &c.AllowPlain,
		&c.AutoRegister, &c.Enabled, &c.CreatedAt, &c.UpdatedAt, &c.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errs.ErrNotFound
	}
	return c, err
}

//nolint:gosec // constraint messages, not credentials
var mqttCredConstraints = map[string]string{
	"mqtt_credentials_username_key":       "this username is taken",
	"mqtt_credentials_tenant_id_name_key": "a credential with this name already exists",
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{2,127}$`)

// DeviceKeyRe is what a topic key may look like: a MAC with or without
// separators, a serial number, an ID.
var DeviceKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// NormalizeDeviceKey is the stored form of a topic key: MACs and IDs
// compare without case.
func NormalizeDeviceKey(k string) string { return strings.ToUpper(k) }

// PasswordParams are the Argon2id parameters new MQTT passwords use.
var PasswordParams = auth.PasswordParams{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1}

func (in *MQTTCredentialInput) validate() error {
	if err := name("name", in.Name); err != nil {
		return err
	}
	if in.Username != "" && !usernameRe.MatchString(in.Username) {
		return errs.Invalidf("username", "3 to 128 letters, digits and . _ : @ -")
	}
	if in.Password != "" && (len(in.Password) < 8 || len(in.Password) > 256) {
		return errs.Invalidf("password", "8 to 256 characters; leave it out to have one generated")
	}
	switch in.Kind {
	case "device":
		if in.DeviceKey == nil || !DeviceKeyRe.MatchString(*in.DeviceKey) {
			return errs.Invalidf("device_key", "a device credential needs the key its topics carry (e.g. the MAC): letters, digits and . _ : -")
		}
		k := NormalizeDeviceKey(*in.DeviceKey)
		in.DeviceKey = &k
	case "shared":
		if in.DeviceKey != nil {
			return errs.Invalidf("device_key", "only device credentials are bound to a key")
		}
	default:
		return errs.Invalidf("kind", "must be device or shared")
	}
	if in.Profile == "" {
		in.Profile = push.ProfileJSON
	}
	if in.Profile != push.ProfileJSON && in.Profile != push.ProfileFixLean {
		return errs.Invalidf("profile", "must be %s or %s", push.ProfileJSON, push.ProfileFixLean)
	}
	return nil
}

// CreateMQTTCredential stores a credential; the password (given or
// generated) is returned once.
func CreateMQTTCredential(ctx context.Context, tx pgx.Tx, actor audit.Actor, tenant uuid.UUID, in MQTTCredentialInput) (MQTTCredential, error) {
	if err := in.validate(); err != nil {
		return MQTTCredential{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return MQTTCredential{}, err
	}
	if in.Username == "" {
		in.Username = "mq-" + strings.ReplaceAll(id.String(), "-", "")[20:]
	}
	password := in.Password
	if password == "" {
		if password, err = auth.GenerateToken(); err != nil {
			return MQTTCredential{}, err
		}
	}
	hash, err := auth.HashPassword(password, PasswordParams)
	if err != nil {
		return MQTTCredential{}, err
	}
	c, err := scanMQTTCredential(tx.QueryRow(ctx, `
		INSERT INTO mqtt_credentials (id, tenant_id, name, username, password_hash, kind, device_key, profile,
		                              allow_plain, auto_register, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+mqttCredCols,
		id, tenant, in.Name, in.Username, hash, in.Kind, in.DeviceKey, in.Profile, in.AllowPlain,
		in.AutoRegister == nil || *in.AutoRegister, in.Enabled == nil || *in.Enabled))
	if err != nil {
		return MQTTCredential{}, errs.FromDB(err, mqttCredConstraints)
	}
	c.Password = password
	return c, audit.Write(ctx, tx, actor.Event(tenant, "mqtt_credential.create", "mqtt_credential", id.String(), nil, c.snapshot()))
}

func (c MQTTCredential) snapshot() map[string]any {
	return map[string]any{"name": c.Name, "username": c.Username, "kind": c.Kind, "device_key": c.DeviceKey,
		"profile": c.Profile, "allow_plain": c.AllowPlain, "auto_register": c.AutoRegister, "enabled": c.Enabled}
}

// ListMQTTCredentials lists the tenant's credentials by name.
func ListMQTTCredentials(ctx context.Context, tx pgx.Tx) ([]MQTTCredential, error) {
	rows, err := tx.Query(ctx, `SELECT `+mqttCredCols+` FROM mqtt_credentials ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MQTTCredential, error) { return scanMQTTCredential(r) })
}

// GetMQTTCredential reads one credential.
func GetMQTTCredential(ctx context.Context, tx pgx.Tx, id uuid.UUID) (MQTTCredential, error) {
	return scanMQTTCredential(tx.QueryRow(ctx, `SELECT `+mqttCredCols+` FROM mqtt_credentials WHERE id = $1`, id))
}

// UpdateMQTTCredential changes the name and the flags. Kind, key, username
// and profile are fixed: create a new credential instead.
func UpdateMQTTCredential(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, name string,
	allowPlain, autoRegister, enabled bool) (MQTTCredential, error) {
	cur, err := scanMQTTCredential(tx.QueryRow(ctx, `SELECT `+mqttCredCols+` FROM mqtt_credentials WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return MQTTCredential{}, err
	}
	if err := nameCheck(name); err != nil {
		return MQTTCredential{}, err
	}
	c, err := scanMQTTCredential(tx.QueryRow(ctx, `
		UPDATE mqtt_credentials SET name = $2, allow_plain = $3, auto_register = $4, enabled = $5, updated_at = now()
		 WHERE id = $1 RETURNING `+mqttCredCols, id, name, allowPlain, autoRegister, enabled))
	if err != nil {
		return MQTTCredential{}, errs.FromDB(err, mqttCredConstraints)
	}
	return c, audit.Write(ctx, tx, actor.Event(c.TenantID, "mqtt_credential.update", "mqtt_credential", id.String(),
		cur.snapshot(), c.snapshot()))
}

func nameCheck(v string) error { return name("name", v) }

// RotateMQTTCredential replaces the password; connected devices keep
// their session until they reconnect.
func RotateMQTTCredential(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, password string) (MQTTCredential, error) {
	if password != "" && (len(password) < 8 || len(password) > 256) {
		return MQTTCredential{}, errs.Invalidf("password", "8 to 256 characters; leave it out to have one generated")
	}
	var err error
	if password == "" {
		if password, err = auth.GenerateToken(); err != nil {
			return MQTTCredential{}, err
		}
	}
	hash, err := auth.HashPassword(password, PasswordParams)
	if err != nil {
		return MQTTCredential{}, err
	}
	c, err := scanMQTTCredential(tx.QueryRow(ctx, `UPDATE mqtt_credentials SET password_hash = $2, updated_at = now()
		WHERE id = $1 RETURNING `+mqttCredCols, id, hash))
	if err != nil {
		return MQTTCredential{}, err
	}
	c.Password = password
	return c, audit.Write(ctx, tx, actor.Event(c.TenantID, "mqtt_credential.rotate", "mqtt_credential", id.String(), nil, nil))
}

// DeleteMQTTCredential removes a credential; devices using it cannot
// connect again.
func DeleteMQTTCredential(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID) error {
	c, err := scanMQTTCredential(tx.QueryRow(ctx, `DELETE FROM mqtt_credentials WHERE id = $1 RETURNING `+mqttCredCols, id))
	if err != nil {
		return err
	}
	return audit.Write(ctx, tx, actor.Event(c.TenantID, "mqtt_credential.delete", "mqtt_credential", id.String(), c.snapshot(), nil))
}

// MQTTDevice binds a device to a topic key, with its data check and its
// connection check.
type MQTTDevice struct {
	TenantID          uuid.UUID
	DeviceKey         string
	DeviceID          uuid.UUID
	DataCheckID       uuid.UUID
	ConnectionCheckID *uuid.UUID
	CreatedAt         time.Time
	Session           *MQTTSession
}

// MQTTSession is a device's current connection.
type MQTTSession struct {
	NodeID      string    `json:"node_id"`
	ClientID    string    `json:"client_id"`
	Remote      string    `json:"remote"`
	Keepalive   int       `json:"keepalive"`
	ConnectedAt time.Time `json:"connected_at"`
}

// GetMQTTDevice reads a device's binding and its current session.
func GetMQTTDevice(ctx context.Context, tx pgx.Tx, device uuid.UUID) (MQTTDevice, error) {
	var m MQTTDevice
	err := tx.QueryRow(ctx, `
		SELECT md.tenant_id, md.device_key, md.device_id, md.data_check_id, md.connection_check_id, md.created_at,
		       (SELECT jsonb_build_object('node_id', s.node_id, 'client_id', s.client_id, 'remote', s.remote,
		               'keepalive', s.keepalive, 'connected_at', s.connected_at) FROM mqtt_sessions s WHERE s.device_id = md.device_id)
		  FROM mqtt_devices md WHERE md.device_id = $1`, device).
		Scan(&m.TenantID, &m.DeviceKey, &m.DeviceID, &m.DataCheckID, &m.ConnectionCheckID, &m.CreatedAt, &m.Session)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, errs.ErrNotFound
	}
	return m, err
}

// MQTT timing: the last-seen rule waits 1.5 × the client's keepalive,
// clamped to 60..300 s, as the FixLean collector did.
func silenceSeconds(keepalive int) int {
	if keepalive <= 0 {
		return 300
	}
	return min(max(keepalive*3/2, 60), 300)
}

// BindMQTT makes a device an MQTT device: a push.mqtt data check (with
// the last-seen rule) and an mqtt.connection check, which becomes the host
// check unless the device already has one. keepalive sets the silence.
func BindMQTT(ctx context.Context, tx pgx.Tx, actor audit.Actor, device uuid.UUID, key, profile string, keepalive int) (MQTTDevice, error) {
	if !DeviceKeyRe.MatchString(key) {
		return MQTTDevice{}, errs.Invalidf("device_key", "letters, digits and . _ : -, at most 64")
	}
	if profile == "" {
		profile = push.ProfileJSON
	}
	if profile != push.ProfileJSON && profile != push.ProfileFixLean {
		return MQTTDevice{}, errs.Invalidf("profile", "must be %s or %s", push.ProfileJSON, push.ProfileFixLean)
	}
	key = NormalizeDeviceKey(key)
	d, err := GetDevice(ctx, tx, device)
	if err != nil {
		return MQTTDevice{}, err
	}
	if _, err := GetMQTTDevice(ctx, tx, device); err == nil {
		return MQTTDevice{}, errs.Conflictf("already-bound", "the device is already bound to MQTT; unbind it first")
	}
	var hasHost bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM checks WHERE device_id = $1 AND is_host_check)`, device).Scan(&hasHost); err != nil {
		return MQTTDevice{}, err
	}
	cfg, _ := json.Marshal(map[string]any{"profile": profile, "missed_count": 1})
	unlimited := Limits{MaxChecks: 1 << 30}
	data, err := createCheck(ctx, tx, actor, device, unlimited, CheckInput{Name: "mqtt data", Plugin: "push.mqtt", Config: cfg,
		IntervalSeconds: silenceSeconds(keepalive), Enabled: true})
	if err != nil {
		return MQTTDevice{}, err
	}
	conn, err := createCheck(ctx, tx, actor, device, unlimited, CheckInput{Name: "mqtt connection", Plugin: "mqtt.connection",
		Enabled: true, IsHostCheck: !hasHost, FailureCount: 1})
	if err != nil {
		return MQTTDevice{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mqtt_devices (tenant_id, device_key, device_id, data_check_id, connection_check_id)
		VALUES ($1, $2, $3, $4, $5)`, d.TenantID, key, device, data.ID, conn.ID); err != nil {
		return MQTTDevice{}, errs.FromDB(err, map[string]string{"mqtt_devices_pkey": "another device already has this key"})
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mqtt_unregistered WHERE tenant_id = $1 AND device_key = $2`, d.TenantID, key); err != nil {
		return MQTTDevice{}, err
	}
	if err := audit.Write(ctx, tx, actor.Event(d.TenantID, "device.mqtt.bind", "device", device.String(), nil,
		map[string]any{"device_key": key, "profile": profile})); err != nil {
		return MQTTDevice{}, err
	}
	return GetMQTTDevice(ctx, tx, device)
}

// UnbindMQTT removes a device's MQTT binding and its two checks.
func UnbindMQTT(ctx context.Context, tx pgx.Tx, actor audit.Actor, device uuid.UUID) error {
	m, err := GetMQTTDevice(ctx, tx, device)
	if err != nil {
		return err
	}
	for _, id := range []*uuid.UUID{&m.DataCheckID, m.ConnectionCheckID} {
		if id == nil {
			continue
		}
		if err := DeleteCheck(ctx, tx, actor, *id); err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mqtt_devices WHERE device_id = $1`, device); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mqtt_sessions WHERE device_id = $1`, device); err != nil {
		return err
	}
	return audit.Write(ctx, tx, actor.Event(m.TenantID, "device.mqtt.unbind", "device", device.String(),
		map[string]any{"device_key": m.DeviceKey}, nil))
}

// AutoRegisterMQTT creates a sensor device for an unknown key and binds
// it. A tenant at its device limit gets errs "limit-reached".
func AutoRegisterMQTT(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, maxDevices int, key, profile, vendor string,
	keepalive int) (MQTTDevice, error) {
	actor := audit.Actor{Kind: audit.ActorSystem, Detail: "mqtt auto-registration"}
	key = NormalizeDeviceKey(key)
	tags := map[string]string{}
	if vendor != "" && len(vendor) <= 100 {
		tags["vendor"] = vendor
	}
	name := key
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE tenant_id = $1 AND name = $2)`, tenant, name).Scan(&taken); err != nil {
		return MQTTDevice{}, err
	}
	if taken {
		name = key + " (mqtt)"
	}
	d, err := CreateDevice(ctx, tx, actor, tenant, maxDevices, DeviceInput{Name: name, Type: "sensor", Tags: tags})
	if err != nil {
		return MQTTDevice{}, err
	}
	return BindMQTT(ctx, tx, actor, d.ID, key, profile, keepalive)
}

// RecordUnregistered counts a message from a key that has no device.
func RecordUnregistered(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, key string, credential uuid.UUID, reason string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO mqtt_unregistered (tenant_id, device_key, credential_id, reason) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, device_key) DO UPDATE SET messages = mqtt_unregistered.messages + 1, last_seen = now(),
		       reason = excluded.reason, credential_id = excluded.credential_id`, tenant, NormalizeDeviceKey(key), credential, reason)
	return err
}

// Unregistered is a key whose messages were dropped.
type Unregistered struct {
	DeviceKey    string     `json:"device_key"`
	CredentialID *uuid.UUID `json:"credential_id"`
	Reason       string     `json:"reason"`
	Messages     int64      `json:"messages"`
	FirstSeen    time.Time  `json:"first_seen"`
	LastSeen     time.Time  `json:"last_seen"`
}

// ListUnregistered lists the tenant's dropped keys, most recent first.
func ListUnregistered(ctx context.Context, tx pgx.Tx) ([]Unregistered, error) {
	rows, err := tx.Query(ctx, `SELECT device_key, credential_id, reason, messages, first_seen, last_seen
		FROM mqtt_unregistered ORDER BY last_seen DESC LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Unregistered])
}
