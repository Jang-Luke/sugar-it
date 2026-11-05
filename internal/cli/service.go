package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
	"sugarintegration/internal/service"
)

// serviceOS is the system the service commands act for; tests change it.
var serviceOS = runtime.GOOS

func (e *env) serviceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Run sugar-it in the background at logon", Args: cobra.ArbitraryArgs, RunE: group}
	cmd.AddCommand(
		&cobra.Command{
			Use: "install", Short: "Install the per-user background service", Args: cobra.NoArgs,
			Long: `Install sugar-it as a per-user background service that starts at logon.

Linux: a systemd user unit, enabled and started now.
macOS: a LaunchAgent, loaded and started now.
Windows: a value in the per-user Run key. It takes effect at your next logon;
it does not start sugar-it now.`,
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
				u, err := service.Plan(serviceOS, exe, home, cfg)
				if err != nil {
					return err
				}
				return service.InstallWith(cmd.Context(), e.svcRunner, u, e.stdout)
			}),
		},
		&cobra.Command{
			Use: "uninstall", Short: "Remove the background service", Args: cobra.NoArgs,
			Long: `Remove the per-user background service.

Linux: stops and disables the systemd user unit, then deletes it.
macOS: unloads the LaunchAgent, then deletes it.
Windows: deletes the Run key value, and the scheduled task sugar-it v0.1.0
created if there is one (that may need an administrator prompt). Windows cannot
stop a process by removing the value, so the command then asks the daemon that
answers for this config directory to stop; a daemon inside the desktop app
keeps running until the app quits.`,
			RunE: plain(func(cmd *cobra.Command, _ []string) error {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				u, err := service.UninstallPlan(serviceOS, home)
				if err != nil {
					return err
				}
				err = service.UninstallWith(cmd.Context(), e.svcRunner, u, e.stdout)
				if serviceOS == "windows" {
					err = errors.Join(err, e.stopRunningDaemon(cmd.Context()))
				}
				return err
			}),
		},
	)
	return cmd
}

// stopRunningDaemon is the Windows half of an uninstall: stop the daemon that
// answers for the config directory, if one does, and say what happened.
func (e *env) stopRunningDaemon(ctx context.Context) error {
	c, err := control.Discover(e.configDir)
	if err != nil {
		if errors.Is(err, control.ErrUnavailable) {
			return service.StopDaemon(ctx, nil, nil, e.stdout)
		}
		return fmt.Errorf("could not look for a running daemon: %w", err)
	}
	return service.StopDaemon(ctx, c, func(ctx context.Context) error {
		return waitStopped(ctx, c, e.configDirOrDefault(), daemon.Grace+5*time.Second)
	}, e.stdout)
}
