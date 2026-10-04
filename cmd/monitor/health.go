package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// newHealth probes a health endpoint and exits non-zero when it is not 200.
// The container image has no shell or curl, so Docker's HEALTHCHECK runs
// this instead.
func newHealth() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Exit 0 when the node's readiness endpoint answers 200 (for container health checks)",
		Args:  cobra.NoArgs,
	}
	url := cmd.Flags().String("url", "http://127.0.0.1:9090/readyz", "endpoint to probe")
	timeout := cmd.Flags().Duration("timeout", 3*time.Second, "request timeout")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
		if err != nil {
			return err
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("%s answered %s", *url, res.Status)
		}
		return nil
	}
	return cmd
}
