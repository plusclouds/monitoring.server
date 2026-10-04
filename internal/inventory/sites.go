// Package inventory stores what the engine monitors: sites, devices, their
// dependencies and their checks (F02, ADR-0014). Every function runs inside
// a tenant-scoped transaction (store.InTenant) and writes its own audit
// event, so the API, file-based configuration (ADR-0015) and bulk imports
// share one code path.
package inventory

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/extref"
)

// ManagedByAPI marks objects created and changed through the API.
const ManagedByAPI = "api"

// Site is a physical or logical location (ADR-0014).
type Site struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Name      string
	Country   *string // ISO 3166-1 alpha-2
	Timezone  *string // IANA name
	Address   *string
	ManagedBy string
	External  *extref.Ref
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SiteInput is the client-writable part of a site. Updates replace it whole.
type SiteInput struct {
	Name     string
	Country  *string
	Timezone *string
	Address  *string
	External *extref.Ref
}

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

func (in SiteInput) validate() error {
	if err := name("name", in.Name); err != nil {
		return err
	}
	if in.Country != nil && !countryRe.MatchString(*in.Country) {
		return errs.Invalidf("country", "must be an ISO 3166-1 alpha-2 code such as TR")
	}
	if in.Timezone != nil {
		if _, err := time.LoadLocation(*in.Timezone); err != nil || *in.Timezone == "" || *in.Timezone == "Local" {
			return errs.Invalidf("timezone", "%q is not an IANA time zone such as Europe/Istanbul", *in.Timezone)
		}
	}
	if in.Address != nil && len(*in.Address) > 1000 {
		return errs.Invalidf("address", "must be at most 1000 characters")
	}
	return in.External.Validate()
}

const siteCols = `id, tenant_id, name, country, timezone, address, managed_by,
	external_source, external_type, external_id, created_at, updated_at`

var siteConstraints = map[string]string{
	"sites_tenant_id_name_key":       "a site with this name already exists",
	"sites_external":                 "a site with this external ID already exists",
	"devices_tenant_id_site_id_fkey": "devices are at this site; move them first",
}

func scanSite(row pgx.Row) (Site, error) {
	var s Site
	var src, typ, ext *string
	err := row.Scan(&s.ID, &s.TenantID, &s.Name, &s.Country, &s.Timezone, &s.Address, &s.ManagedBy,
		&src, &typ, &ext, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, errs.ErrNotFound
	}
	s.External = extref.FromColumns(src, typ, ext)
	return s, err
}

func (s Site) snapshot() map[string]any {
	return map[string]any{"name": s.Name, "country": s.Country, "timezone": s.Timezone,
		"address": s.Address, "external": s.External}
}

// GetSite returns one site.
func GetSite(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Site, error) {
	return scanSite(tx.QueryRow(ctx, `SELECT `+siteCols+` FROM sites WHERE id = $1`, id))
}

// LockSite returns a site locked for update.
func LockSite(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Site, error) {
	return scanSite(tx.QueryRow(ctx, `SELECT `+siteCols+` FROM sites WHERE id = $1 FOR UPDATE`, id))
}

// SiteByExternal finds a site by external ID.
func SiteByExternal(ctx context.Context, tx pgx.Tx, k extref.Key) (Site, error) {
	return one(ctx, tx, `SELECT `+siteCols+` FROM sites WHERE `+extref.Where(1)+` LIMIT 2`, k.Args(), scanSite)
}

// ListSites returns sites ordered by ID.
func ListSites(ctx context.Context, tx pgx.Tx, p Page, ext *extref.Key) ([]Site, error) {
	src, typ, id := keyColumns(ext)
	rows, err := tx.Query(ctx, `SELECT `+siteCols+` FROM sites
		 WHERE ($1::text IS NULL OR external_source = $1)
		   AND ($2::text IS NULL OR external_type = $2)
		   AND ($3::text IS NULL OR external_id = $3)
		   AND ($4::uuid IS NULL OR id > $4)
		 ORDER BY id LIMIT $5`, src, typ, id, p.After, p.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Site, error) { return scanSite(r) })
}

// CreateSite adds a site.
func CreateSite(ctx context.Context, tx pgx.Tx, actor audit.Actor, tenant uuid.UUID, in SiteInput) (Site, error) {
	if err := in.validate(); err != nil {
		return Site{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Site{}, err
	}
	src, typ, ext := in.External.Columns()
	s, err := scanSite(tx.QueryRow(ctx, `
		INSERT INTO sites (id, tenant_id, name, country, timezone, address, external_source, external_type, external_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+siteCols,
		id, tenant, in.Name, in.Country, in.Timezone, in.Address, src, typ, ext))
	if err != nil {
		return Site{}, errs.FromDB(err, siteConstraints)
	}
	return s, audit.Write(ctx, tx, actor.Event(tenant, "site.create", "site", id.String(), nil, s.snapshot()))
}

// UpdateSite replaces a site's fields. An update that changes nothing
// writes no audit event.
func UpdateSite(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, in SiteInput) (Site, error) {
	if err := in.validate(); err != nil {
		return Site{}, err
	}
	cur, err := scanSite(tx.QueryRow(ctx, `SELECT `+siteCols+` FROM sites WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return Site{}, err
	}
	if err := writable(cur.ManagedBy); err != nil {
		return Site{}, err
	}
	next := cur
	next.Name, next.Country, next.Timezone, next.Address, next.External = in.Name, in.Country, in.Timezone, in.Address, in.External
	if reflect.DeepEqual(cur.snapshot(), next.snapshot()) {
		return cur, nil
	}
	src, typ, ext := in.External.Columns()
	out, err := scanSite(tx.QueryRow(ctx, `
		UPDATE sites SET name = $2, country = $3, timezone = $4, address = $5,
		       external_source = $6, external_type = $7, external_id = $8, updated_at = now()
		 WHERE id = $1 RETURNING `+siteCols, id, in.Name, in.Country, in.Timezone, in.Address, src, typ, ext))
	if err != nil {
		return Site{}, errs.FromDB(err, siteConstraints)
	}
	return out, audit.Write(ctx, tx, actor.Event(cur.TenantID, "site.update", "site", id.String(), cur.snapshot(), out.snapshot()))
}

// DeleteSite removes a site without devices.
func DeleteSite(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID) error {
	cur, err := GetSite(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := writable(cur.ManagedBy); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sites WHERE id = $1`, id); err != nil {
		return errs.FromDB(err, siteConstraints)
	}
	return audit.Write(ctx, tx, actor.Event(cur.TenantID, "site.delete", "site", id.String(), cur.snapshot(), nil))
}
