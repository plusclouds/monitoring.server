package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/crypto"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/extref"
)

// Endpoint is a webhook receiver, without its secrets.
type Endpoint struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	Name              string
	URL               string
	Enabled           bool
	DisabledReason    *string
	TimeoutSeconds    int
	HeaderNames       []string
	PreviousExpiresAt *time.Time
	ManagedBy         string
	External          *extref.Ref
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// EndpointInput is the client-writable part of an endpoint. Headers that
// are not sent keep their values, like credential secrets.
type EndpointInput struct {
	Name           string
	URL            string
	Enabled        bool
	TimeoutSeconds int // 0 = 10
	Headers        map[string]*string
	External       *extref.Ref
}

// secrets is the encrypted part of an endpoint.
type secrets struct {
	Secret   string            `json:"secret"`
	Previous string            `json:"previous,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// Store reads and writes endpoints and routes inside tenant-scoped
// transactions.
type Store struct {
	Keys *crypto.Keyring
}

var reservedHeaders = []string{"content-type", "content-length", "host", "user-agent", "webhook-id",
	"webhook-timestamp", "webhook-signature", "transfer-encoding", "connection"}

func (in *EndpointInput) validate() error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 {
		return errs.Invalidf("name", "must be 1 to 200 characters")
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(in.URL) > 2000 {
		return errs.Invalidf("url", "must be an http(s) URL")
	}
	if u.User != nil {
		return errs.Invalidf("url", "must not contain credentials; use headers")
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 10
	}
	if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 30 {
		return errs.Invalidf("timeout_seconds", "must be between 1 and 30")
	}
	if len(in.Headers) > 20 {
		return errs.Invalidf("headers", "at most 20 headers")
	}
	for name, v := range in.Headers {
		if !validHeaderName(name) || slices.Contains(reservedHeaders, strings.ToLower(name)) {
			return errs.Invalidf("headers", "%q is not an allowed header name", name)
		}
		if v != nil && (len(*v) > 1000 || strings.ContainsAny(*v, "\r\n")) {
			return errs.Invalidf("headers."+name, "must be at most 1000 characters on one line")
		}
	}
	return in.External.Validate()
}

func validHeaderName(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, c := range s {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if c != '-' && (c < '0' || c > '9') && !letter {
			return false
		}
	}
	return true
}

const endpointCols = `id, tenant_id, name, url, enabled, disabled_reason, timeout_seconds, header_names,
	previous_expires_at, managed_by, external_source, external_type, external_id, created_at, updated_at`

var endpointConstraints = map[string]string{
	"webhook_endpoints_tenant_id_name_key":    "a webhook endpoint with this name already exists",
	"webhook_endpoints_external":              "a webhook endpoint with this external ID already exists",
	"alert_routes_tenant_id_endpoint_id_fkey": "alert routes send to this endpoint; change them first",
}

func scanEndpoint(row pgx.Row) (Endpoint, error) {
	var e Endpoint
	var src, typ, ext *string
	err := row.Scan(&e.ID, &e.TenantID, &e.Name, &e.URL, &e.Enabled, &e.DisabledReason, &e.TimeoutSeconds,
		&e.HeaderNames, &e.PreviousExpiresAt, &e.ManagedBy, &src, &typ, &ext, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, errs.ErrNotFound
	}
	e.External = extref.FromColumns(src, typ, ext)
	return e, err
}

func (e Endpoint) snapshot() map[string]any {
	return map[string]any{"name": e.Name, "url": e.URL, "enabled": e.Enabled, "timeout_seconds": e.TimeoutSeconds,
		"header_names": e.HeaderNames, "external": e.External}
}

// GetEndpoint returns one endpoint.
func GetEndpoint(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Endpoint, error) {
	return scanEndpoint(tx.QueryRow(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE id = $1`, id))
}

// EndpointByExternal finds an endpoint by external ID.
func EndpointByExternal(ctx context.Context, tx pgx.Tx, k extref.Key) (Endpoint, error) {
	rows, err := tx.Query(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE `+extref.Where(1)+` LIMIT 2`, k.Args()...)
	if err != nil {
		return Endpoint{}, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Endpoint, error) { return scanEndpoint(r) })
	switch {
	case err != nil:
		return Endpoint{}, err
	case len(list) == 0:
		return Endpoint{}, errs.ErrNotFound
	case len(list) > 1:
		return Endpoint{}, extref.ErrAmbiguous
	}
	return list[0], nil
}

// ListEndpoints returns every endpoint of the tenant, by name.
func ListEndpoints(ctx context.Context, tx pgx.Tx) ([]Endpoint, error) {
	rows, err := tx.Query(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Endpoint, error) { return scanEndpoint(r) })
}

// CreateEndpoint stores an endpoint and returns its new signing secret,
// which is shown once.
func (s *Store) CreateEndpoint(ctx context.Context, tx pgx.Tx, actor audit.Actor, tenant uuid.UUID, in EndpointInput) (Endpoint, string, error) {
	if err := in.validate(); err != nil {
		return Endpoint{}, "", err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Endpoint{}, "", err
	}
	secret, err := NewSecret()
	if err != nil {
		return Endpoint{}, "", err
	}
	sec := secrets{Secret: secret, Headers: mergeHeaders(nil, in.Headers)}
	sealed, err := s.seal(tenant, id, sec)
	if err != nil {
		return Endpoint{}, "", err
	}
	src, typ, ext := in.External.Columns()
	e, err := scanEndpoint(tx.QueryRow(ctx, `
		INSERT INTO webhook_endpoints (id, tenant_id, name, url, enabled, timeout_seconds, header_names, ciphertext,
		                               nonce, wrapped_dek, kek_id, external_source, external_type, external_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING `+endpointCols,
		id, tenant, in.Name, in.URL, in.Enabled, in.TimeoutSeconds, headerNames(sec.Headers), sealed.Ciphertext,
		sealed.Nonce, sealed.WrappedDEK, sealed.KEKID, src, typ, ext))
	if err != nil {
		return Endpoint{}, "", errs.FromDB(err, endpointConstraints)
	}
	return e, secret, audit.Write(ctx, tx, actor.Event(tenant, "webhook.create", "webhook", id.String(), nil, e.snapshot()))
}

// UpdateEndpoint replaces an endpoint's fields. Enabling it again clears
// the reason it was disabled.
func (s *Store) UpdateEndpoint(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, in EndpointInput) (Endpoint, error) {
	if err := in.validate(); err != nil {
		return Endpoint{}, err
	}
	cur, err := scanEndpoint(tx.QueryRow(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return Endpoint{}, err
	}
	sec, err := s.secrets(ctx, tx, cur)
	if err != nil {
		return Endpoint{}, err
	}
	next := cur
	next.Name, next.URL, next.Enabled, next.TimeoutSeconds, next.External = in.Name, in.URL, in.Enabled, in.TimeoutSeconds, in.External
	headers := mergeHeaders(sec.Headers, in.Headers)
	next.HeaderNames = headerNames(headers)
	headersChanged := !reflect.DeepEqual(headers, nonNilMap(sec.Headers))
	before, after := cur.snapshot(), next.snapshot()
	if reflect.DeepEqual(before, after) && !headersChanged {
		return cur, nil
	}
	sec.Headers = headers
	sealed, err := s.seal(cur.TenantID, id, sec)
	if err != nil {
		return Endpoint{}, err
	}
	src, typ, ext := next.External.Columns()
	out, err := scanEndpoint(tx.QueryRow(ctx, `
		UPDATE webhook_endpoints SET name = $2, url = $3, enabled = $4,
		       disabled_reason = CASE WHEN $4 THEN NULL ELSE disabled_reason END, timeout_seconds = $5,
		       header_names = $6, ciphertext = $7, nonce = $8, wrapped_dek = $9, kek_id = $10,
		       external_source = $11, external_type = $12, external_id = $13, updated_at = now()
		 WHERE id = $1 RETURNING `+endpointCols,
		id, next.Name, next.URL, next.Enabled, next.TimeoutSeconds, next.HeaderNames, sealed.Ciphertext, sealed.Nonce,
		sealed.WrappedDEK, sealed.KEKID, src, typ, ext))
	if err != nil {
		return Endpoint{}, errs.FromDB(err, endpointConstraints)
	}
	if headersChanged {
		after["headers_changed"] = true
	}
	return out, audit.Write(ctx, tx, actor.Event(cur.TenantID, "webhook.update", "webhook", id.String(), before, after))
}

// DeleteEndpoint removes an endpoint no route uses; its deliveries go too.
func DeleteEndpoint(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID) error {
	cur, err := GetEndpoint(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM webhook_endpoints WHERE id = $1`, id); err != nil {
		return errs.FromDB(err, endpointConstraints)
	}
	return audit.Write(ctx, tx, actor.Event(cur.TenantID, "webhook.delete", "webhook", id.String(), cur.snapshot(), nil))
}

// RotateSecret makes a new signing secret. The old one stays valid for
// overlap: deliveries carry both signatures until then (ADR-0009).
func (s *Store) RotateSecret(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, overlap time.Duration) (Endpoint, string, error) {
	cur, err := scanEndpoint(tx.QueryRow(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return Endpoint{}, "", err
	}
	sec, err := s.secrets(ctx, tx, cur)
	if err != nil {
		return Endpoint{}, "", err
	}
	secret, err := NewSecret()
	if err != nil {
		return Endpoint{}, "", err
	}
	sec.Previous, sec.Secret = sec.Secret, secret
	sealed, err := s.seal(cur.TenantID, id, sec)
	if err != nil {
		return Endpoint{}, "", err
	}
	out, err := scanEndpoint(tx.QueryRow(ctx, `
		UPDATE webhook_endpoints SET ciphertext = $2, nonce = $3, wrapped_dek = $4, kek_id = $5,
		       previous_expires_at = now() + make_interval(secs => $6), updated_at = now()
		 WHERE id = $1 RETURNING `+endpointCols,
		id, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKID, overlap.Seconds()))
	if err != nil {
		return Endpoint{}, "", err
	}
	return out, secret, audit.Write(ctx, tx, actor.Event(cur.TenantID, "webhook.rotate_secret", "webhook", id.String(),
		nil, map[string]any{"previous_valid_until": out.PreviousExpiresAt}))
}

// Signing is what a delivery needs from an endpoint's secrets.
type Signing struct {
	Secrets []string // current first, then the previous one while it is valid
	Headers map[string]string
}

// SigningFor decrypts an endpoint's secrets. q may be any role's pool or
// transaction that can see the row.
func (s *Store) SigningFor(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id uuid.UUID) (Signing, error) {
	var e Endpoint
	var sealed crypto.Sealed
	err := q.QueryRow(ctx, `SELECT tenant_id, previous_expires_at, ciphertext, nonce, wrapped_dek, kek_id
		FROM webhook_endpoints WHERE id = $1`, id).Scan(&e.TenantID, &e.PreviousExpiresAt, &sealed.Ciphertext,
		&sealed.Nonce, &sealed.WrappedDEK, &sealed.KEKID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Signing{}, errs.ErrNotFound
	}
	if err != nil {
		return Signing{}, err
	}
	e.ID = id
	sec, err := s.open(e, sealed)
	if err != nil {
		return Signing{}, err
	}
	out := Signing{Secrets: []string{sec.Secret}, Headers: sec.Headers}
	if sec.Previous != "" && e.PreviousExpiresAt != nil && time.Now().Before(*e.PreviousExpiresAt) {
		out.Secrets = append(out.Secrets, sec.Previous)
	}
	return out, nil
}

func (s *Store) secrets(ctx context.Context, tx pgx.Tx, e Endpoint) (secrets, error) {
	var sealed crypto.Sealed
	if err := tx.QueryRow(ctx, `SELECT ciphertext, nonce, wrapped_dek, kek_id FROM webhook_endpoints WHERE id = $1`, e.ID).
		Scan(&sealed.Ciphertext, &sealed.Nonce, &sealed.WrappedDEK, &sealed.KEKID); err != nil {
		return secrets{}, err
	}
	return s.open(e, sealed)
}

func aad(tenant, id uuid.UUID) []byte { return crypto.AAD(tenant.String(), id.String(), "webhook") }

func (s *Store) seal(tenant, id uuid.UUID, sec secrets) (crypto.Sealed, error) {
	// Marshalled only to be encrypted right away (ADR-0006).
	b, err := json.Marshal(sec) //nolint:gosec // the secret is sealed below, never stored or logged in clear
	if err != nil {
		return crypto.Sealed{}, err
	}
	return s.Keys.Seal(b, aad(tenant, id))
}

func (s *Store) open(e Endpoint, sealed crypto.Sealed) (secrets, error) {
	b, err := s.Keys.Open(sealed, aad(e.TenantID, e.ID))
	if err != nil {
		return secrets{}, err
	}
	var out secrets
	return out, json.Unmarshal(b, &out)
}

// mergeHeaders applies an update: sent headers are set (nil removes),
// others keep their values; a nil update map keeps everything.
func mergeHeaders(cur map[string]string, in map[string]*string) map[string]string {
	out := nonNilMap(maps.Clone(cur))
	for k, v := range in {
		k = http.CanonicalHeaderKey(k)
		if v == nil || *v == "" {
			delete(out, k)
		} else {
			out[k] = *v
		}
	}
	return out
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func headerNames(h map[string]string) []string {
	return append([]string{}, slices.Sorted(maps.Keys(h))...)
}
