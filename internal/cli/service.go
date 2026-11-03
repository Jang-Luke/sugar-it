package cli

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"sugarintegration/internal/service"
)

func (e *env) serviceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Run sugar-it in the background at logon", Args: cobra.ArbitraryArgs, RunE: group}
	cmd.AddCommand(
		&cobra.Command{
			Use: "install", Short: "Install and start the per-user background service", Args: cobra.NoArgs,
			RunE: plain(func(cmd *cobra.Command, _ []string) error {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				cfg := e.configDir
				if cfg != "" {
					if cfg, err = filepath.Abs(cfg); err != nil {
						return err
					}
				}
				u, err := service.Plan(runtime.GOOS, exe, home, cfg)
				if err != nil {
					return err
				}
				return service.InstallWith(cmd.Context(), e.svcRunner, u, e.stdout)
			}),
		},
		&cobra.Command{
			Use: "uninstall", Short: "Stop and remove the background service", Args: cobra.NoArgs,
			RunE: plain(func(cmd *cobra.Command, _ []string) error {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				u, err := service.UninstallPlan(runtime.GOOS, home)
				if err != nil {
					return err
				}
				return service.UninstallWith(cmd.Context(), e.svcRunner, u, e.stdout)
			}),
		},
	)
	return cmd
}
