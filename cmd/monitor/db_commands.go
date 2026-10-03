package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/plusclouds/monitoring.server/internal/admin"
	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/auth"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/store"
)

// dsn resolves one of the database DSNs, requiring it to be set.
func dsn(name string, d config.DSN) (string, error) {
	v, err := config.ReadSecret(name+".dsn", d.DSN, d.DSNFile)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("%s.dsn_file is required for this command", name)
	}
	return v, nil
}

// openSystem connects as the system role, used by the admin commands.
func openSystem(ctx context.Context, c config.Config) (*pgxpool.Pool, error) {
	d, err := dsn("database.system", c.Database.System)
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, store.PoolOptions{
		DSN: d, MaxConns: 2, ConnectTimeout: c.Database.ConnectTimeout.D(), AppName: "monitor-admin",
	})
}

func newMigrate() *cobra.Command {
	cmd := &cobra.Command{Use: "migrate", Short: "Apply or inspect database migrations (as the schema owner)"}
	for _, dir := range []string{"up", "down", "status"} {
		sub := &cobra.Command{Use: dir, Args: cobra.NoArgs}
		switch dir {
		case "up":
			sub.Short = "Apply every pending migration"
		case "down":
			sub.Short = "Roll back the most recent migration"
		case "status":
			sub.Short = "List migrations and whether they are applied"
		}
		path := configFlag(sub, defaultConfigPath)
		sub.RunE = func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load(*path, nil)
			if err != nil {
				return err
			}
			owner, err := dsn("database.owner", c.Database.Owner)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if dir == "status" {
				st, err := store.MigrationStatus(cmd.Context(), owner)
				if err != nil {
					return err
				}
				for _, s := range st {
					if _, err := fmt.Fprintf(out, "%05d  %-8s  %s\n", s.Source.Version, s.State, s.Source.Path); err != nil {
						return err
					}
				}
				return nil
			}
			res, err := store.Migrate(cmd.Context(), owner, dir)
			for _, r := range res {
				if _, werr := fmt.Fprintf(out, "%-4s %05d  %s  (%s)\n", r.Direction, r.Source.Version, r.Source.Path, r.Duration); werr != nil {
					return werr
				}
			}
			if err == nil && len(res) == 0 {
				_, err = fmt.Fprintln(out, "nothing to do")
			}
			return err
		}
		cmd.AddCommand(sub)
	}
	return cmd
}

func newAdmin() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Administrative commands that run outside the API"}
	cmd.AddCommand(newBootstrap(), newVerifyAudit(), newGenToken())
	return cmd
}

func newBootstrap() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create the installation ID, the platform tenant and the first key (runs once)",
		Long: `Without flags, creates the platform key that the PlusClouds API (leo4) uses.
With --standalone --tenant NAME, creates a local tenant and an admin API key instead,
for installs without PlusClouds. Keys are printed once and cannot be shown again.
With --if-needed, a second run succeeds without changes, so deployments can run it on every start.`,
		Args: cobra.NoArgs,
	}
	path := configFlag(cmd, defaultConfigPath)
	standalone := cmd.Flags().Bool("standalone", false, "create a local tenant and admin key instead of a platform key")
	tenant := cmd.Flags().String("tenant", "", "name of the local tenant (with --standalone)")
	ifNeeded := cmd.Flags().Bool("if-needed", false,
		"succeed without changes when already bootstrapped (for automated deployments)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := config.Load(*path, nil)
		if err != nil {
			return err
		}
		db, err := openSystem(cmd.Context(), c)
		if err != nil {
			return err
		}
		defer db.Close()
		res, err := admin.Bootstrap(cmd.Context(), db, admin.BootstrapOptions{
			Standalone: *standalone, TenantName: *tenant, TenantDefaults: c.Platform.TenantDefaults,
		})
		if errors.Is(err, admin.ErrAlreadyBootstrapped) && *ifNeeded {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "already bootstrapped; nothing to do")
			return err
		}
		if err != nil {
			return err
		}
		return printBootstrap(cmd.OutOrStdout(), res)
	}
	return cmd
}

func printBootstrap(w io.Writer, r admin.BootstrapResult) error {
	lines := []string{
		fmt.Sprintf("installation id:    %s", r.InstallationID),
		fmt.Sprintf("platform tenant id: %s", r.PlatformTenantID),
	}
	if r.PlatformKey != "" {
		lines = append(lines, "", "Platform key for the PlusClouds API (shown once, store it in leo4's secrets):", r.PlatformKey)
	}
	if r.AdminKey != "" {
		lines = append(lines, fmt.Sprintf("tenant id:          %s", r.TenantID), "",
			"Admin API key (shown once):", r.AdminKey)
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l); err != nil {
			return err
		}
	}
	return nil
}

func newVerifyAudit() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify-audit",
		Short: "Check the audit log hash chain of every tenant (or one with --tenant)",
		Args:  cobra.NoArgs,
	}
	path := configFlag(cmd, defaultConfigPath)
	tenant := cmd.Flags().String("tenant", "", "tenant ID to check (default: all)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		var only *uuid.UUID
		if *tenant != "" {
			id, err := uuid.Parse(*tenant)
			if err != nil {
				return fmt.Errorf("--tenant: %w", err)
			}
			only = &id
		}
		c, err := config.Load(*path, nil)
		if err != nil {
			return err
		}
		db, err := openSystem(cmd.Context(), c)
		if err != nil {
			return err
		}
		defer db.Close()
		problems, err := audit.Verify(cmd.Context(), db, only)
		if err != nil {
			return err
		}
		// Record the check itself, including its outcome (F01: admin commands are audited).
		if err := admin.RecordCLI(cmd.Context(), db, "audit.verify", map[string]any{
			"tenant": *tenant, "problems": len(problems),
		}); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		for _, p := range problems {
			if _, err := fmt.Fprintf(out, "tenant %s: event %d: %s\n", p.TenantID, p.Seq, p.Detail); err != nil {
				return err
			}
		}
		if len(problems) > 0 {
			return errors.New("audit chain verification failed")
		}
		_, err = fmt.Fprintln(out, "audit chain intact")
		return err
	}
	return cmd
}

func newGenToken() *cobra.Command {
	return &cobra.Command{
		Use:   "gen-token",
		Short: "Print a random token for status_token_file or preshared enrollment tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := auth.GenerateToken()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), t)
			return err
		},
	}
}
