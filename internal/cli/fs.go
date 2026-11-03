package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"sugarintegration/internal/control"
	"sugarintegration/internal/vfs"
)

// target parses a "peer:share/path" argument; a bad one is a usage error.
func target(s string, needShare bool) (control.Target, error) {
	t, err := control.ParseTarget(s)
	if err != nil {
		return t, usageErr(err)
	}
	if needShare && t.Share == "" {
		return t, usagef("%q: name a share as <peer>:<share>/<path>", s)
	}
	return t, nil
}

func (e *env) lsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls <peer>[:<share>[/path]]",
		Short: "List the shares of a host, or the contents of a folder",
		Args:  cobra.ExactArgs(1),
		RunE: plain(func(cmd *cobra.Command, args []string) error {
			t, err := target(args[0], false)
			if err != nil {
				return err
			}
			c, err := e.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if t.Share == "" {
				list, err := c.ListShares(ctx, t.Peer)
				if err != nil {
					return err
				}
				return e.printShares(list)
			}
			entries, err := c.List(ctx, t)
			if err != nil {
				return err
			}
			if entries == nil {
				entries = []vfs.Entry{}
			}
			if e.json {
				return printJSON(e.stdout, entries)
			}
			tw := newTable(e.stdout)
			for _, en := range entries {
				name, size := en.Name, humanSize(en.Size)
				switch en.Type {
				case vfs.TypeDir:
					name, size = name+"/", "-"
				case vfs.TypeSymlink: // not followed, so no size of its own to show
					name, size = name+"@", "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", size, humanTime(en.ModTime), name)
			}
			return tw.Flush()
		}),
	}
}

func (e *env) rmCmd() *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:   "rm [-r] <target>",
		Short: "Delete a file, or a folder with -r",
		Args:  cobra.ExactArgs(1),
		RunE: plain(func(cmd *cobra.Command, args []string) error {
			t, err := target(args[0], true)
			if err != nil {
				return err
			}
			return e.withClient(cmd.Context(), func(ctx context.Context, c *control.Client) error {
				return c.Remove(ctx, t, recursive)
			})
		}),
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "delete a folder and everything in it")
	return cmd
}

func (e *env) mkdirCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mkdir <target>",
		Short: "Create a folder",
		Args:  cobra.ExactArgs(1),
		RunE: plain(func(cmd *cobra.Command, args []string) error {
			t, err := target(args[0], true)
			if err != nil {
				return err
			}
			return e.withClient(cmd.Context(), func(ctx context.Context, c *control.Client) error {
				return c.Mkdir(ctx, t)
			})
		}),
	}
}

func (e *env) renameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <target> <new-name>",
		Short: "Rename a file or folder inside its folder",
		Args:  cobra.ExactArgs(2),
		RunE: plain(func(cmd *cobra.Command, args []string) error {
			t, err := target(args[0], true)
			if err != nil {
				return err
			}
			return e.withClient(cmd.Context(), func(ctx context.Context, c *control.Client) error {
				return c.Rename(ctx, t, args[1])
			})
		}),
	}
}

func (e *env) withClient(ctx context.Context, f func(context.Context, *control.Client) error) error {
	c, err := e.client()
	if err != nil {
		return err
	}
	return f(ctx, c)
}
