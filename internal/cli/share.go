package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"sugarintegration/internal/control"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/shares"
)

func (e *env) shareCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "share", Short: "Manage the shares of this host", Args: cobra.ArbitraryArgs, RunE: group}
	var readOnly bool
	add := &cobra.Command{
		Use: "add <name> <path>", Short: "Share a folder", Args: cobra.ExactArgs(2),
		RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
			abs, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			return c.AddShare(ctx, shares.Spec{Name: args[0], Path: abs, ReadOnly: readOnly})
		}),
	}
	add.Flags().BoolVar(&readOnly, "read-only", false, "peers cannot change this share")
	cmd.AddCommand(add,
		&cobra.Command{
			Use: "list", Short: "List the shares of this host", Args: cobra.NoArgs,
			RunE: e.run(func(ctx context.Context, c *control.Client, _ []string) error {
				list, err := c.ListShares(ctx, "local")
				if err != nil {
					return err
				}
				return e.printShares(list)
			}),
		},
		&cobra.Command{
			Use: "remove <name>", Short: "Stop sharing a folder", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				return c.RemoveShare(ctx, args[0])
			}),
		},
	)
	return cmd
}

func (e *env) printShares(list []peerapi.ShareInfo) error {
	if list == nil {
		list = []peerapi.ShareInfo{}
	}
	if e.json {
		return printJSON(e.stdout, list)
	}
	tw := newTable(e.stdout)
	fmt.Fprintln(tw, "SHARE\tMODE")
	for _, s := range list {
		mode := "read-write"
		if s.ReadOnly {
			mode = "read-only"
		}
		fmt.Fprintf(tw, "%s\t%s\n", s.Name, mode)
	}
	return tw.Flush()
}
