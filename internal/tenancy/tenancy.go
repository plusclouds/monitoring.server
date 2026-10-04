// Package tenancy stores tenants, the user mirror and memberships (F01,
// ADR-0012) and implements provisioning by external ID.
//
// Reads that cross tenants go through the SECURITY DEFINER functions of
// migration 00002. Writes run in a transaction scoped to the affected tenant,
// so row-level security checks them (ADR-0008).
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound means the tenant, user or membership does not exist (or is not
// visible to the caller).
var ErrNotFound = errors.New("not found")

// ValidationError is a request value the database would reject.
type ValidationError struct {
	Field, Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// Querier is a pool or a transaction.
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Tenant statuses.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusDeleted   = "deleted"
	StatusPurged    = "purged" // data removed after the purge grace; a tombstone for the audit log
)

// Gone reports whether the tenant was deleted or purged.
func (t Tenant) Gone() bool { return t.Status == StatusDeleted || t.Status == StatusPurged }

type Tenant struct {
	ID             uuid.UUID
	Name           string
	Status         string
	IsPlatform     bool
	ConfigSource   string
	Limits         Limits
	ExternalSource *string
	ExternalType   *string
	ExternalID     *string
	Provisioned    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
}

type Limits struct {
	MaxDevices              int
	MaxChecks               int
	MinCheckIntervalSeconds int
	APIRatePerMinute        int
	AllowedTargetNetworks   []netip.Prefix
	MetricClasses           []string
	MaxAPIKeyLifetimeDays   *int
}

// LimitsPatch changes only the fields that are set.
type LimitsPatch struct {
	MaxDevices              *int
	MaxChecks               *int
	MinCheckIntervalSeconds *int
	APIRatePerMinute        *int
	AllowedTargetNetworks   *[]string
	MetricClasses           *[]string
	MaxAPIKeyLifetimeDays   *int
}

// Apply returns l with the patch applied, validating network strings.
func (p LimitsPatch) Apply(l Limits) (Limits, error) {
	set := func(dst *int, src *int) {
		if src != nil {
			*dst = *src
		}
	}
	set(&l.MaxDevices, p.MaxDevices)
	set(&l.MaxChecks, p.MaxChecks)
	set(&l.MinCheckIntervalSeconds, p.MinCheckIntervalSeconds)
	set(&l.APIRatePerMinute, p.APIRatePerMinute)
	if p.MaxAPIKeyLifetimeDays != nil {
		v := *p.MaxAPIKeyLifetimeDays
		l.MaxAPIKeyLifetimeDays = &v
	}
	if p.MetricClasses != nil {
		l.MetricClasses = append([]string(nil), (*p.MetricClasses)...)
	}
	if p.AllowedTargetNetworks != nil {
		nets, err := ParseNetworks(*p.AllowedTargetNetworks)
		if err != nil {
			return l, err
		}
		l.AllowedTargetNetworks = nets
	}
	return l, nil
}

// ParseNetworks parses CIDR strings for allowed_target_networks.
func ParseNetworks(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, &ValidationError{"limits.allowed_target_networks", fmt.Sprintf("%q is not a CIDR", s)}
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

const tenantCols = `id, name, status, is_platform, config_source, max_devices, max_checks,
	min_check_interval_seconds, api_rate_per_minute, allowed_target_networks, metric_classes,
	max_api_key_lifetime_days, external_source, external_type, external_id, provisioned,
	created_at, updated_at, deleted_at`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Name, &t.Status, &t.IsPlatform, &t.ConfigSource,
		&t.Limits.MaxDevices, &t.Limits.MaxChecks, &t.Limits.MinCheckIntervalSeconds,
		&t.Limits.APIRatePerMinute, &t.Limits.AllowedTargetNetworks, &t.Limits.MetricClasses,
		&t.Limits.MaxAPIKeyLifetimeDays, &t.ExternalSource, &t.ExternalType, &t.ExternalID,
		&t.Provisioned, &t.CreatedAt, &t.UpdatedAt, &t.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// ByID finds a tenant across tenants.
func ByID(ctx context.Context, q Querier, id uuid.UUID) (Tenant, error) {
	return scanTenant(q.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenant_by_id($1)`, id))
}

// ByExternal finds a tenant by its external ID across tenants.
func ByExternal(ctx context.Context, q Querier, source, id string) (Tenant, error) {
	return scanTenant(q.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenant_by_external($1, $2)`, source, id))
}

// PlatformID is the platform tenant, which holds platform-level audit events.
func PlatformID(ctx context.Context, q Querier) (uuid.UUID, error) {
	var id *uuid.UUID
	if err := q.QueryRow(ctx, `SELECT platform_tenant_id()`).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	if id == nil {
		return uuid.Nil, errors.New("installation is not bootstrapped")
	}
	return *id, nil
}

// List returns customer tenants ordered by ID, after the given cursor.
func List(ctx context.Context, q Querier, source, externalID *string, after *uuid.UUID, limit int) ([]Tenant, error) {
	rows, err := q.Query(ctx, `SELECT `+tenantCols+` FROM list_tenants($1, $2, $3, $4)`, source, externalID, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) { return scanTenant(r) })
}

// Snapshot is the audited form of a tenant.
func (t Tenant) Snapshot() map[string]any {
	nets := make([]string, len(t.Limits.AllowedTargetNetworks))
	for i, n := range t.Limits.AllowedTargetNetworks {
		nets[i] = n.String()
	}
	return map[string]any{
		"name": t.Name, "status": t.Status,
		"limits": map[string]any{
			"max_devices": t.Limits.MaxDevices, "max_checks": t.Limits.MaxChecks,
			"min_check_interval_seconds": t.Limits.MinCheckIntervalSeconds,
			"api_rate_per_minute":        t.Limits.APIRatePerMinute,
			"allowed_target_networks":    nets, "metric_classes": t.Limits.MetricClasses,
			"max_api_key_lifetime_days": t.Limits.MaxAPIKeyLifetimeDays,
		},
		"external_type": t.ExternalType,
	}
}

// insert writes a new tenant inside a transaction scoped to t.ID. It returns
// false when another request created the same external ID first.
func insert(ctx context.Context, tx pgx.Tx, t Tenant) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO tenants (id, name, status, max_devices, max_checks, min_check_interval_seconds,
		                     api_rate_per_minute, allowed_target_networks, metric_classes,
		                     max_api_key_lifetime_days, external_source, external_type, external_id, provisioned)
		VALUES ($1, $2, 'active', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (external_source, external_id) WHERE external_id IS NOT NULL DO NOTHING`,
		t.ID, t.Name, t.Limits.MaxDevices, t.Limits.MaxChecks, t.Limits.MinCheckIntervalSeconds,
		t.Limits.APIRatePerMinute, nonNilPrefixes(t.Limits.AllowedTargetNetworks), nonNilStrings(t.Limits.MetricClasses),
		t.Limits.MaxAPIKeyLifetimeDays, t.ExternalSource, t.ExternalType, t.ExternalID, t.Provisioned)
	if err != nil {
		return false, mapError(err)
	}
	return tag.RowsAffected() == 1, nil
}

// lockForUpdate reads the tenant row inside its scoped transaction.
func lockForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Tenant, error) {
	return scanTenant(tx.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = $1 FOR UPDATE`, id))
}

func update(ctx context.Context, tx pgx.Tx, t Tenant) (Tenant, error) {
	return scanTenant(tx.QueryRow(ctx, `
		UPDATE tenants SET name = $2, status = $3, max_devices = $4, max_checks = $5,
		       min_check_interval_seconds = $6, api_rate_per_minute = $7, allowed_target_networks = $8,
		       metric_classes = $9, max_api_key_lifetime_days = $10, external_type = $11,
		       deleted_at = CASE WHEN $3 = 'deleted' THEN coalesce(deleted_at, now()) END,
		       updated_at = now()
		 WHERE id = $1
		RETURNING `+tenantCols,
		t.ID, t.Name, t.Status, t.Limits.MaxDevices, t.Limits.MaxChecks, t.Limits.MinCheckIntervalSeconds,
		t.Limits.APIRatePerMinute, nonNilPrefixes(t.Limits.AllowedTargetNetworks), nonNilStrings(t.Limits.MetricClasses),
		t.Limits.MaxAPIKeyLifetimeDays, t.ExternalType))
}

// mapError turns constraint violations into validation errors.
func mapError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23514" { // check_violation
		return &ValidationError{Field: pg.ConstraintName, Message: "value out of range"}
	}
	return err
}

func nonNilPrefixes(p []netip.Prefix) []netip.Prefix {
	if p == nil {
		return []netip.Prefix{}
	}
	return p
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
