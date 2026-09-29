// Package admin implements the `monitor admin` commands that run outside
// the API. They write audit events like API writes do (F01).
package admin

import (
	"context"
	"errors"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/auth"
	"github.com/plusclouds/monitoring.server/internal/config"
)

// ErrAlreadyBootstrapped is returned when bootstrap ran before.
var ErrAlreadyBootstrapped = errors.New("this installation is already bootstrapped")

// BootstrapOptions choose between a PlusClouds-connected install (platform
// key for leo4) and a standalone one (a local tenant with an admin key).
type BootstrapOptions struct {
	Standalone     bool
	TenantName     string              // standalone only
	TenantDefaults config.TenantLimits // limits of the standalone tenant
}

// BootstrapResult carries the keys, which are shown once and never stored.
type BootstrapResult struct {
	InstallationID   uuid.UUID
	PlatformTenantID uuid.UUID
	PlatformKey      string // PlusClouds mode
	TenantID         uuid.UUID
	AdminKey         string // standalone mode
}

// Bootstrap creates the installation ID, the platform tenant and the first
// key, in one transaction, using the system database role.
func Bootstrap(ctx context.Context, db *pgxpool.Pool, o BootstrapOptions) (BootstrapResult, error) {
	var res BootstrapResult
	if o.Standalone && o.TenantName == "" {
		return res, errors.New("--tenant is required with --standalone")
	}
	host, _ := os.Hostname()

	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var err error
		if res.InstallationID, err = uuid.NewV7(); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO installation (id) VALUES ($1) ON CONFLICT DO NOTHING`, res.InstallationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyBootstrapped
		}

		cli := func(tenant uuid.UUID, action, objType, objID string, after any) error {
			return audit.Write(ctx, tx, audit.Event{
				TenantID: tenant, ActorKind: audit.ActorAdminCLI, ActorDetail: host,
				Action: action, ObjectType: objType, ObjectID: objID, After: after,
			})
		}

		// The platform tenant holds self-monitoring and PlusClouds staff checks.
		// It has no plan limits.
		if res.PlatformTenantID, err = createTenant(ctx, tx, "platform", true, config.TenantLimits{
			MaxDevices: 1 << 30, MaxChecks: 1 << 30, MinCheckIntervalSeconds: 5, APIRatePerMinute: 0,
		}); err != nil {
			return err
		}
		if err := cli(res.PlatformTenantID, "installation.bootstrap", "installation", res.InstallationID.String(),
			map[string]any{"standalone": o.Standalone}); err != nil {
			return err
		}
		if err := cli(res.PlatformTenantID, "tenant.create", "tenant", res.PlatformTenantID.String(),
			map[string]any{"name": "platform", "is_platform": true}); err != nil {
			return err
		}

		if !o.Standalone {
			id, key, err := createKey(ctx, tx, nil, "platform", "leo4", "platform")
			if err != nil {
				return err
			}
			res.PlatformKey = key
			return cli(res.PlatformTenantID, "api_key.create", "api_key", id.String(),
				map[string]any{"name": "leo4", "kind": "platform"})
		}

		if res.TenantID, err = createTenant(ctx, tx, o.TenantName, false, o.TenantDefaults); err != nil {
			return err
		}
		if err := cli(res.TenantID, "tenant.create", "tenant", res.TenantID.String(),
			map[string]any{"name": o.TenantName}); err != nil {
			return err
		}
		id, key, err := createKey(ctx, tx, &res.TenantID, "tenant", "bootstrap admin", "admin")
		if err != nil {
			return err
		}
		res.AdminKey = key
		return cli(res.TenantID, "api_key.create", "api_key", id.String(),
			map[string]any{"name": "bootstrap admin", "role": "admin"})
	})
	return res, err
}

func createTenant(ctx context.Context, tx pgx.Tx, name string, platform bool, l config.TenantLimits) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return id, err
	}
	classes := l.MetricClasses
	if len(classes) == 0 {
		classes = []string{"standard"}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO tenants (id, name, is_platform, max_devices, max_checks, min_check_interval_seconds,
		                     api_rate_per_minute, allowed_target_networks, metric_classes, provisioned)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::cidr[], $9, 'bootstrap')`,
		id, name, platform, l.MaxDevices, l.MaxChecks, l.MinCheckIntervalSeconds, l.APIRatePerMinute,
		nonNil(l.AllowedTargetNetworks), classes)
	return id, err
}

func createKey(ctx context.Context, tx pgx.Tx, tenant *uuid.UUID, kind, name, role string) (uuid.UUID, string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return id, "", err
	}
	k, err := auth.GenerateKey()
	if err != nil {
		return id, "", err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO api_keys (id, tenant_id, kind, name, prefix, hash, role)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, tenant, kind, name, k.Prefix, k.Hash, role)
	return id, k.Full, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// RecordCLI writes an audit event for an admin command in the platform
// tenant, for commands that change nothing themselves (verify-audit).
func RecordCLI(ctx context.Context, db *pgxpool.Pool, action string, after any) error {
	host, _ := os.Hostname()
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var platform uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE is_platform`).Scan(&platform); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("not bootstrapped: run `monitor admin bootstrap` first")
			}
			return err
		}
		return audit.Write(ctx, tx, audit.Event{
			TenantID: platform, ActorKind: audit.ActorAdminCLI, ActorDetail: host,
			Action: action, ObjectType: "installation", After: after,
		})
	})
}
