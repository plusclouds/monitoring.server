package admin

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plusclouds/monitoring.server/internal/audit"
)

// NewKey is a key created from the command line. Key is shown once and
// never stored; only its hash is.
type NewKey struct {
	ID       uuid.UUID
	TenantID uuid.UUID // the platform tenant for a platform key
	Key      string
	Revoked  int64 // older keys revoked by the same command
}

// RotatePlatformKey creates a new platform key, for when the key from
// bootstrap is lost or leaked. Unless keepOld is set, every other platform
// key is revoked in the same transaction; with keepOld the old keys keep
// working until RevokePlatformKeys, so the PlusClouds API can switch over
// without downtime.
func RotatePlatformKey(ctx context.Context, db *pgxpool.Pool, keepOld bool) (NewKey, error) {
	var out NewKey
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var err error
		if out.TenantID, err = platformTenant(ctx, tx); err != nil {
			return err
		}
		if out.ID, out.Key, err = createKey(ctx, tx, nil, "platform", "leo4", "platform"); err != nil {
			return err
		}
		if !keepOld {
			if out.Revoked, err = revokePlatformKeys(ctx, tx, out.ID); err != nil {
				return err
			}
		}
		return cliEvent(ctx, tx, out.TenantID, "api_key.rotate", out.ID,
			map[string]any{"name": "leo4", "kind": "platform", "revoked_others": out.Revoked})
	})
	return out, err
}

// RevokePlatformKeys revokes every platform key except keep, after a
// rotation with keepOld. Platform keys cannot be revoked through the API.
func RevokePlatformKeys(ctx context.Context, db *pgxpool.Pool, keep uuid.UUID) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		platform, err := platformTenant(ctx, tx)
		if err != nil {
			return err
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys
			WHERE id = $1 AND kind = 'platform' AND revoked_at IS NULL)`, keep).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s is not an active platform key; nothing revoked", keep)
		}
		if n, err = revokePlatformKeys(ctx, tx, keep); err != nil {
			return err
		}
		return cliEvent(ctx, tx, platform, "api_key.revoke", keep, map[string]any{"kind": "platform", "kept": keep, "revoked": n})
	})
	return n, err
}

func revokePlatformKeys(ctx context.Context, tx pgx.Tx, keep uuid.UUID) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now()
		WHERE kind = 'platform' AND revoked_at IS NULL AND id <> $1`, keep)
	return tag.RowsAffected(), err
}

// CreateAdminKey creates an admin API key for a tenant, for standalone
// installs whose bootstrap admin key is lost. Existing keys stay; revoke
// them through the API with the new key.
func CreateAdminKey(ctx context.Context, db *pgxpool.Pool, tenant uuid.UUID) (NewKey, error) {
	out := NewKey{TenantID: tenant}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := platformTenant(ctx, tx); err != nil {
			return err
		}
		var platform bool
		var status string
		err := tx.QueryRow(ctx, `SELECT is_platform, status FROM tenants WHERE id = $1`, tenant).Scan(&platform, &status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("tenant %s does not exist", tenant)
		case err != nil:
			return err
		case platform:
			return errors.New("the platform tenant has no admin keys; use rotate-platform-key")
		case status == "purged":
			return fmt.Errorf("tenant %s is purged", tenant)
		}
		if out.ID, out.Key, err = createKey(ctx, tx, &tenant, "tenant", "admin (CLI)", "admin"); err != nil {
			return err
		}
		return cliEvent(ctx, tx, tenant, "api_key.create", out.ID, map[string]any{"name": "admin (CLI)", "role": "admin"})
	})
	return out, err
}

func platformTenant(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE is_platform`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, errors.New("not bootstrapped: run `monitor admin bootstrap` first")
	}
	return id, err
}

func cliEvent(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, action string, key uuid.UUID, after any) error {
	host, _ := os.Hostname()
	return audit.Write(ctx, tx, audit.Event{
		TenantID: tenant, ActorKind: audit.ActorAdminCLI, ActorDetail: host,
		Action: action, ObjectType: "api_key", ObjectID: key.String(), After: after,
	})
}
