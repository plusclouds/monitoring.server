package inventory

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/extref"
)

// Page is a cursor page: rows with an ID after After, at most Limit.
type Page struct {
	After *uuid.UUID
	Limit int
}

// one runs a query expected to return at most one row; two rows mean an
// ambiguous external ID.
func one[T any](ctx context.Context, tx pgx.Tx, sql string, args []any, scan func(pgx.Row) (T, error)) (T, error) {
	var zero T
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return zero, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return scan(r) })
	switch {
	case err != nil:
		return zero, err
	case len(list) == 0:
		return zero, errs.ErrNotFound
	case len(list) > 1:
		return zero, extref.ErrAmbiguous
	}
	return list[0], nil
}

// keyColumns returns a list filter's parts; unset parts match anything.
func keyColumns(k *extref.Key) (src, typ, id *string) {
	if k == nil {
		return nil, nil, nil
	}
	if k.Source != "" {
		src = &k.Source
	}
	if k.ID != "" {
		id = &k.ID
	}
	return src, k.Type, id
}

// name checks a required object name.
func name(field, v string) error {
	if strings.TrimSpace(v) == "" || len(v) > 200 {
		return errs.Invalidf(field, "must be 1 to 200 characters")
	}
	return nil
}

// writable refuses changes to objects another writer owns (ADR-0015):
// objects from a configuration file, or discovered by a collector.
func writable(managedBy string) error {
	switch managedBy {
	case ManagedByAPI:
		return nil
	case "file":
		return errs.Conflictf("managed-by-file", "this object is managed by the tenant's configuration file; change it there")
	default:
		return errs.Conflictf("managed-by-collector", "this object is maintained by %s; only tags, notes and external IDs can change", managedBy)
	}
}

// lockTenant serializes writes of one kind within a tenant for the rest of
// the transaction: limit checks and cycle checks need a stable view.
func lockTenant(ctx context.Context, tx pgx.Tx, kind string, tenant uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, kind+":"+tenant.String())
	return err
}
