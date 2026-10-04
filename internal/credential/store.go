package credential

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/crypto"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/extref"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Credential is a stored credential without its secrets.
type Credential struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Name        string
	Type        string
	Fields      map[string]string // non-secret fields
	SecretNames []string          // which secret fields are set
	ManagedBy   string
	External    *extref.Ref
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Input creates or changes a credential. In Fields, a nil value clears an
// optional field; a field left out is unchanged on update.
type Input struct {
	Name     *string
	Type     string // required on create; must match on update
	Fields   map[string]*string
	External *extref.Ref
}

// Store reads and writes credentials inside a tenant-scoped transaction.
type Store struct {
	Keys *crypto.Keyring
}

const cols = `id, tenant_id, name, type, fields, secret_names, managed_by,
	external_source, external_type, external_id, created_at, updated_at`

// constraintMessages explains constraint violations; no secrets here.
var constraintMessages = map[string]string{ //nolint:gosec // constraint names, not credentials
	"credentials_tenant_id_name_key": "a credential with this name already exists",
	"credentials_external":           "a credential with this external ID already exists",
	"check_credentials_tenant_id_credential_id_fkey": "the credential is used by checks; " +
		"change those checks first",
}

func scan(row pgx.Row) (Credential, error) {
	var c Credential
	var src, typ, ext *string
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Type, &c.Fields, &c.SecretNames, &c.ManagedBy,
		&src, &typ, &ext, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errs.ErrNotFound
	}
	c.External = extref.FromColumns(src, typ, ext)
	return c, err
}

// Get returns one credential.
func Get(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Credential, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM credentials WHERE id = $1`, id))
}

// ByExternal finds a credential by its external ID.
func ByExternal(ctx context.Context, tx pgx.Tx, k extref.Key) (Credential, error) {
	rows, err := tx.Query(ctx, `SELECT `+cols+` FROM credentials WHERE `+extref.Where(1)+` LIMIT 2`, k.Args()...)
	if err != nil {
		return Credential{}, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Credential, error) { return scan(r) })
	switch {
	case err != nil:
		return Credential{}, err
	case len(list) == 0:
		return Credential{}, errs.ErrNotFound
	case len(list) > 1:
		return Credential{}, extref.ErrAmbiguous
	}
	return list[0], nil
}

// Filter narrows List.
type Filter struct {
	Type     *string
	External *extref.Key
	After    *uuid.UUID
	Limit    int
}

// List returns credentials ordered by ID.
func List(ctx context.Context, tx pgx.Tx, f Filter) ([]Credential, error) {
	var src, typ, ext *string
	if k := f.External; k != nil {
		typ = k.Type
		if k.Source != "" {
			src = &k.Source
		}
		if k.ID != "" {
			ext = &k.ID
		}
	}
	rows, err := tx.Query(ctx, `SELECT `+cols+` FROM credentials
		 WHERE ($1::text IS NULL OR type = $1)
		   AND ($2::text IS NULL OR external_source = $2)
		   AND ($3::text IS NULL OR external_type = $3)
		   AND ($4::text IS NULL OR external_id = $4)
		   AND ($5::uuid IS NULL OR id > $5)
		 ORDER BY id LIMIT $6`, f.Type, src, typ, ext, f.After, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Credential, error) { return scan(r) })
}

// Create stores a new credential.
func (s *Store) Create(ctx context.Context, tx pgx.Tx, actor audit.Actor, tenant uuid.UUID, in Input) (Credential, error) {
	t, ok := Lookup(in.Type)
	if !ok {
		return Credential{}, errs.Invalidf("type", "unknown credential type %q", in.Type)
	}
	if in.Name == nil || *in.Name == "" {
		return Credential{}, errs.Invalidf("name", "is required")
	}
	if err := in.External.Validate(); err != nil {
		return Credential{}, err
	}
	v, err := t.split(in.Fields)
	if err != nil {
		return Credential{}, err
	}
	if err := t.check(v); err != nil {
		return Credential{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Credential{}, err
	}
	sealed, err := s.seal(tenant, id, t.Name, v.Secrets)
	if err != nil {
		return Credential{}, err
	}
	src, typ, ext := in.External.Columns()
	c, err := scan(tx.QueryRow(ctx, `
		INSERT INTO credentials (id, tenant_id, name, type, fields, secret_names, ciphertext, nonce,
		                         wrapped_dek, kek_id, external_source, external_type, external_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+cols,
		id, tenant, *in.Name, t.Name, v.Fields, sortedKeys(v.Secrets), sealed.Ciphertext, sealed.Nonce,
		sealed.WrappedDEK, sealed.KEKID, src, typ, ext))
	if err != nil {
		return Credential{}, errs.FromDB(err, constraintMessages)
	}
	after := c.snapshot()
	after["secrets_changed"] = sortedKeys(v.Secrets)
	return c, audit.Write(ctx, tx, actor.Event(tenant, "credential.create", "credential", id.String(), nil, after))
}

// Update changes a credential. Secret fields are merged: one that is not
// sent keeps its value. An update that changes nothing writes no audit event.
func (s *Store) Update(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, in Input) (Credential, error) {
	cur, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM credentials WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return Credential{}, err
	}
	if in.Type != "" && in.Type != cur.Type {
		return Credential{}, errs.Conflictf("type-change", "a credential's type cannot change; create a new one")
	}
	if err := in.External.Validate(); err != nil {
		return Credential{}, err
	}
	t, _ := Lookup(cur.Type)
	secrets, err := s.secrets(ctx, tx, cur)
	if err != nil {
		return Credential{}, err
	}
	merged := map[string]*string{}
	for k, v := range cur.Fields {
		merged[k] = &v
	}
	for k, v := range secrets {
		merged[k] = &v
	}
	maps.Copy(merged, in.Fields)
	v, err := t.split(merged)
	if err != nil {
		return Credential{}, err
	}
	if err := t.check(v); err != nil {
		return Credential{}, err
	}
	next := cur
	if in.Name != nil {
		next.Name = *in.Name
	}
	if in.External != nil {
		next.External = in.External
	}
	next.Fields, next.SecretNames = v.Fields, sortedKeys(v.Secrets)
	changedSecrets := changedKeys(secrets, v.Secrets)
	before, after := cur.snapshot(), next.snapshot()
	if reflect.DeepEqual(before, after) && len(changedSecrets) == 0 {
		return cur, nil
	}
	sealed, err := s.seal(cur.TenantID, cur.ID, cur.Type, v.Secrets)
	if err != nil {
		return Credential{}, err
	}
	src, typ, ext := next.External.Columns()
	out, err := scan(tx.QueryRow(ctx, `
		UPDATE credentials SET name = $2, fields = $3, secret_names = $4, ciphertext = $5, nonce = $6,
		       wrapped_dek = $7, kek_id = $8, external_source = $9, external_type = $10, external_id = $11,
		       updated_at = now()
		 WHERE id = $1 RETURNING `+cols,
		id, next.Name, next.Fields, next.SecretNames, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK,
		sealed.KEKID, src, typ, ext))
	if err != nil {
		return Credential{}, errs.FromDB(err, constraintMessages)
	}
	if len(changedSecrets) > 0 {
		after["secrets_changed"] = changedSecrets
	}
	return out, audit.Write(ctx, tx, actor.Event(cur.TenantID, "credential.update", "credential", id.String(), before, after))
}

// Delete removes a credential that no check uses.
func Delete(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID) error {
	cur, err := Get(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM credentials WHERE id = $1`, id); err != nil {
		return errs.FromDB(err, constraintMessages)
	}
	return audit.Write(ctx, tx, actor.Event(cur.TenantID, "credential.delete", "credential", id.String(), cur.snapshot(), nil))
}

// Decrypt returns the credential with its secrets, for a plugin run. q may
// be any role's pool or transaction that can see the row.
func (s *Store) Decrypt(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id uuid.UUID) (plugin.Credential, error) {
	var c Credential
	var sealed crypto.Sealed
	err := q.QueryRow(ctx, `SELECT tenant_id, type, fields, ciphertext, nonce, wrapped_dek, kek_id
		FROM credentials WHERE id = $1`, id).Scan(&c.TenantID, &c.Type, &c.Fields,
		&sealed.Ciphertext, &sealed.Nonce, &sealed.WrappedDEK, &sealed.KEKID)
	if errors.Is(err, pgx.ErrNoRows) {
		return plugin.Credential{}, errs.ErrNotFound
	}
	if err != nil {
		return plugin.Credential{}, err
	}
	c.ID = id
	secrets, err := s.open(c, sealed)
	if err != nil {
		return plugin.Credential{}, err
	}
	out := plugin.Credential{Type: c.Type, Fields: c.Fields, Secret: make(map[string]plugin.Secret, len(secrets))}
	for k, v := range secrets {
		out.Secret[k] = plugin.Secret(v)
	}
	return out, nil
}

// Rewrap re-wraps every data key not wrapped by the active KEK
// (`monitor admin rewrap-credentials`). It needs a role that sees all
// tenants and returns how many credentials changed.
func (s *Store) Rewrap(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, `SELECT id, ciphertext, nonce, wrapped_dek, kek_id FROM credentials
		WHERE kek_id <> $1 FOR UPDATE`, s.Keys.Active())
	if err != nil {
		return 0, err
	}
	type item struct {
		id uuid.UUID
		s  crypto.Sealed
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var it item
		err := r.Scan(&it.id, &it.s.Ciphertext, &it.s.Nonce, &it.s.WrappedDEK, &it.s.KEKID)
		return it, err
	})
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		re, err := s.Keys.Rewrap(it.s)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE credentials SET wrapped_dek = $2, kek_id = $3 WHERE id = $1`,
			it.id, re.WrappedDEK, re.KEKID); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}

func (s *Store) secrets(ctx context.Context, tx pgx.Tx, c Credential) (map[string]string, error) {
	var sealed crypto.Sealed
	if err := tx.QueryRow(ctx, `SELECT ciphertext, nonce, wrapped_dek, kek_id FROM credentials WHERE id = $1`, c.ID).
		Scan(&sealed.Ciphertext, &sealed.Nonce, &sealed.WrappedDEK, &sealed.KEKID); err != nil {
		return nil, err
	}
	return s.open(c, sealed)
}

func aad(tenant, id uuid.UUID, typ string) []byte {
	return crypto.AAD(tenant.String(), id.String(), typ)
}

func (s *Store) seal(tenant, id uuid.UUID, typ string, secrets map[string]string) (crypto.Sealed, error) {
	b, err := json.Marshal(secrets)
	if err != nil {
		return crypto.Sealed{}, err
	}
	return s.Keys.Seal(b, aad(tenant, id, typ))
}

func (s *Store) open(c Credential, sealed crypto.Sealed) (map[string]string, error) {
	b, err := s.Keys.Open(sealed, aad(c.TenantID, c.ID, c.Type))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	return out, json.Unmarshal(b, &out)
}

// snapshot is the audited form: secrets appear only by name.
func (c Credential) snapshot() map[string]any {
	return map[string]any{
		"name": c.Name, "type": c.Type, "fields": c.Fields, "secrets_set": c.SecretNames, "external": c.External,
	}
}

func sortedKeys(m map[string]string) []string {
	return append([]string{}, slices.Sorted(maps.Keys(m))...)
}

func changedKeys(before, after map[string]string) []string {
	var out []string
	for k, v := range after {
		if before[k] != v {
			out = append(out, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
