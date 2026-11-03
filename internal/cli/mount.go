package cli

import (
	"context"
	"runtime"

	"github.com/spf13/cobra"

	"sugarintegration/internal/control"
	"sugarintegration/internal/mount"
)

func (e *env) mountCmd() *cobra.Command {
	var drive string
	cmd := &cobra.Command{
		Use:   "mount [--drive Z:]",
		Short: "Mount the mesh as a network drive",
		Args:  cobra.NoArgs,
		RunE: e.run(func(ctx context.Context, c *control.Client, _ []string) error {
			plan, err := mount.MountPlan(runtime.GOOS, c.DAVAddr(), c.Token(), drive, e.winLimits())
			if err != nil {
				return err
			}
			if plan.Stdin == "" {
				// The OS asks for the password itself; show it on the terminal
				// only, never in the command line.
				plan.Notes = append(plan.Notes, "Token (password): "+c.Token())
			}
			return mount.RunWith(ctx, e.mountRunner, plan, e.stdout)
		}),
	}
	cmd.Flags().StringVar(&drive, "drive", "", "Windows drive letter such as Z: (default: any free drive)")
	return cmd
}

func (e *env) unmountCmd() *cobra.Command {
	var drive string
	cmd := &cobra.Command{
		Use:   "unmount [--drive Z:]",
		Short: "Unmount the network drive",
		Args:  cobra.NoArgs,
		RunE: e.run(func(ctx context.Context, c *control.Client, _ []string) error {
			plan, err := mount.UnmountPlan(runtime.GOOS, c.DAVAddr(), drive)
			if err != nil {
				return err
			}
			return mount.RunWith(ctx, e.mountRunner, plan, e.stdout)
		}),
	}
	cmd.Flags().StringVar(&drive, "drive", "", "Windows drive letter to disconnect, such as Z:")
	return cmd
}

func (e *env) winLimits() mount.WinLimits {
	if runtime.GOOS != "windows" {
		return mount.WinLimits{}
	}
	return mount.ReadWinLimits()
}
