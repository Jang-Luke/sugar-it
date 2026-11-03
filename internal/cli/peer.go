package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"sugarintegration/internal/control"
	"sugarintegration/internal/node"
)

func (e *env) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show this host and the peers it knows",
		Args:  cobra.NoArgs,
		RunE: e.run(func(ctx context.Context, c *control.Client, _ []string) error {
			st, err := c.Status(ctx)
			if err != nil {
				return err
			}
			if st.Peers == nil {
				st.Peers = []node.PeerStatus{}
			}
			if e.json {
				return printJSON(e.stdout, st)
			}
			fmt.Fprintf(e.stdout, "name:     %s\nid:       %s\nversion:  %s\n", st.Name, st.ID, st.Version)
			e.printPeers(st.Peers)
			return nil
		}),
	}
}

func (e *env) printPeers(peers []node.PeerStatus) {
	if len(peers) == 0 {
		fmt.Fprintln(e.stdout, "no peers; pair one with `sugar-it peer add <invite>`")
		return
	}
	tw := newTable(e.stdout)
	fmt.Fprintln(tw, "NAME\tSTATE\tLAST SEEN\tADDRESSES")
	for _, p := range peers {
		state := "offline"
		switch {
		case p.Incompatible:
			state = "incompatible"
		case p.Online:
			state = "online"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\n", p.Name, state, humanTime(p.LastSeen), p.Addrs)
	}
	tw.Flush()
}

func (e *env) inviteCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "invite",
		Short: "Print an invitation for another host to join with `peer add`",
		Args:  cobra.NoArgs,
		RunE: e.run(func(ctx context.Context, c *control.Client, _ []string) error {
			inv, err := c.Invite(ctx, ttl)
			if err != nil {
				return err
			}
			fmt.Fprintln(e.stdout, inv)
			return nil
		}),
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the invitation is valid (default 10m)")
	return cmd
}

func (e *env) peerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "peer", Short: "Manage the hosts of the mesh", Args: cobra.ArbitraryArgs, RunE: group}
	cmd.AddCommand(
		&cobra.Command{
			Use: "add <invite>", Short: "Join a host with its invitation", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				p, err := c.JoinPeer(ctx, args[0])
				if err != nil {
					return err
				}
				fmt.Fprintf(e.stdout, "paired with %s\n", p.Name)
				return nil
			}),
		},
		&cobra.Command{
			Use: "list", Short: "List the hosts", Args: cobra.NoArgs,
			RunE: e.run(func(ctx context.Context, c *control.Client, _ []string) error {
				st, err := c.Status(ctx)
				if err != nil {
					return err
				}
				if st.Peers == nil {
					st.Peers = []node.PeerStatus{}
				}
				if e.json {
					return printJSON(e.stdout, st.Peers)
				}
				e.printPeers(st.Peers)
				return nil
			}),
		},
		&cobra.Command{
			Use: "remove <name>", Short: "Remove a host from the mesh", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				return c.RemovePeer(ctx, args[0])
			}),
		},
		&cobra.Command{
			Use: "rename <new-name>", Short: "Rename this host", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				return c.RenameSelf(ctx, args[0])
			}),
		},
	)
	return cmd
}
