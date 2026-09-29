// Command monitor is the monitoring engine: one binary with switchable roles
// (ADR-0001).
package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/plusclouds/monitoring.server/internal/app"
	"github.com/plusclouds/monitoring.server/internal/buildinfo"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/logging"
)

const defaultConfigPath = "/etc/monitor/config.yaml"

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "monitor",
		Short:         "API-first monitoring engine",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServe(), newConfigCmd(), newVersion())
	return root
}

// configFlag registers --config, defaulting to MONITOR_CONFIG or the
// standard path.
func configFlag(cmd *cobra.Command, def string) *string {
	if env := os.Getenv("MONITOR_CONFIG"); env != "" {
		def = env
	}
	return cmd.Flags().String("config", def, "config file (empty for defaults and environment only)")
}

// loadCore loads the config and resolves the roles: --roles wins over node.roles.
func loadCore(path, rolesFlag string) (config.Config, []string, error) {
	c, err := config.Load(path, nil)
	if err != nil {
		return c, nil, err
	}
	roles := c.Node.Roles
	if rolesFlag != "" {
		roles = nil
		for r := range strings.SplitSeq(rolesFlag, ",") {
			roles = append(roles, strings.TrimSpace(r))
		}
	}
	return c, roles, c.Validate(roles)
}

func newServe() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the engine roles",
		Args:  cobra.NoArgs,
	}
	path := configFlag(cmd, defaultConfigPath)
	roles := cmd.Flags().String("roles", "", "comma-separated roles to run (default: node.roles)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, rs, err := loadCore(*path, *roles)
		if err != nil {
			return err
		}
		log, err := logging.New(os.Stderr, c.Logging.Format, c.Logging.Level)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return app.Serve(ctx, app.ServeOptions{
			Config: c,
			Roles:  rs,
			Logger: log,
			Reload: func() (config.Config, error) {
				c, _, err := loadCore(*path, *roles)
				return c, err
			},
		})
	}
	return cmd
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Check and show configuration"}

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate a config file for the given roles, or a probe config with --probe",
		Args:  cobra.NoArgs,
	}
	vPath := configFlag(validate, defaultConfigPath)
	vRoles := validate.Flags().String("roles", "", "roles to validate for (default: node.roles)")
	vProbe := validate.Flags().Bool("probe", false, "the file is a probe config")
	validate.RunE = func(cmd *cobra.Command, _ []string) error {
		if *vProbe {
			p, err := config.LoadProbe(*vPath, nil)
			if err != nil {
				return err
			}
			if err := p.Validate(); err != nil {
				return err
			}
		} else if _, _, err := loadCore(*vPath, *vRoles); err != nil {
			return err
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "config is valid")
		return err
	}

	print := &cobra.Command{
		Use:   "print",
		Short: "Print the effective config (file, defaults and environment) with secrets redacted",
		Args:  cobra.NoArgs,
	}
	pPath := configFlag(print, defaultConfigPath)
	pProbe := print.Flags().Bool("probe", false, "the file is a probe config")
	print.RunE = func(cmd *cobra.Command, _ []string) error {
		var out any
		if *pProbe {
			p, err := config.LoadProbe(*pPath, nil)
			if err != nil {
				return err
			}
			out = config.Redacted(p)
		} else {
			c, err := config.Load(*pPath, nil)
			if err != nil {
				return err
			}
			out = config.Redacted(c)
		}
		enc := yaml.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent(2)
		return errors.Join(enc.Encode(out), enc.Close())
	}

	cmd.AddCommand(validate, print)
	return cmd
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "monitor %s (%s)\n", buildinfo.Version, buildinfo.CommitOrVCS())
			return err
		},
	}
}
