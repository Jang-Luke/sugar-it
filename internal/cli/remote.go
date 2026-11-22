package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"sugarintegration/internal/control"
	"sugarintegration/internal/node"
)

func (e *env) remoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Remote screen control (over Tailscale by default)",
		Long: "Let chosen peers reach this host's screen service (macOS Screen Sharing, or the\n" +
			"sugar-it remote agent on Windows), or connect to a peer's. Settings can only be\n" +
			"changed on the host itself.",
		Args: cobra.ArbitraryArgs, RunE: group,
	}
	set := func(s node.RemoteSettings) func(ctx context.Context, c *control.Client, _ []string) error {
		return func(ctx context.Context, c *control.Client, _ []string) error { return c.SetRemote(ctx, s) }
	}
	on, off := true, false
	var listen string
	connect := &cobra.Command{
		Use: "connect <peer>", Short: "Forward a local port to a peer's screen service until interrupted", Args: cobra.ExactArgs(1),
		RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
			return c.RemoteConnect(ctx, args[0], listen, func(ev control.RemoteEvent) {
				switch ev.Event {
				case "listening":
					fmt.Fprintf(e.stdout, "%s\n", ev.Addr)
					fmt.Fprintf(e.stderr, "Point a VNC viewer at %s. Press Ctrl+C to stop.\n", ev.Addr)
				case "open":
					fmt.Fprintln(e.stderr, "viewer connected")
				case "close":
					fmt.Fprintln(e.stderr, "viewer disconnected")
				case "error":
					fmt.Fprintf(e.stderr, "error: %s\n", ev.Message)
				}
			})
		}),
	}
	connect.Flags().StringVar(&listen, "listen", "", "local loopback address (default 127.0.0.1 and a free port)")
	cmd.AddCommand(
		&cobra.Command{
			Use: "status", Short: "Show this host's remote-control settings", Args: cobra.NoArgs,
			RunE: e.run(func(ctx context.Context, c *control.Client, _ []string) error {
				st, err := c.RemoteStatus(ctx)
				if err != nil {
					return err
				}
				return e.printRemote(st)
			}),
		},
		&cobra.Command{Use: "enable", Short: "Allow remote control of this host", Args: cobra.NoArgs, RunE: e.run(set(node.RemoteSettings{Enabled: &on}))},
		&cobra.Command{Use: "disable", Short: "Refuse all remote control of this host", Args: cobra.NoArgs, RunE: e.run(set(node.RemoteSettings{Enabled: &off}))},
		&cobra.Command{
			Use: "allow <peer>", Short: "Let a peer control this host (until it is removed or re-paired)", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error { return c.AllowRemote(ctx, args[0]) }),
		},
		&cobra.Command{
			Use: "deny <peer>", Short: "Take a peer (name or ID) off the allow list", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error { return c.DenyRemote(ctx, args[0]) }),
		},
		&cobra.Command{
			Use: "target <host:port>", Short: `Set the local screen service address ("" for the system default)`, Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				return c.SetRemote(ctx, node.RemoteSettings{Target: &args[0]})
			}),
		},
		&cobra.Command{
			Use: "networks [cidr...]", Short: "Set the networks remote control may use (none: Tailscale only)", Args: cobra.ArbitraryArgs,
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				nets := append([]string{}, args...)
				return c.SetRemote(ctx, node.RemoteSettings{Networks: &nets})
			}),
		},
		connect,
	)
	return cmd
}

func (e *env) printRemote(st node.RemoteStatus) error {
	if e.json {
		return printJSON(e.stdout, st)
	}
	state := "disabled"
	if st.Enabled {
		state = "enabled"
	}
	target := st.Target
	switch {
	case target == "":
		target = "(none: set one with `sugar-it remote target`)"
	case st.DefaultTarget:
		target += " (system default)"
	}
	fmt.Fprintf(e.stdout, "remote control: %s\ntarget: %s\nnetworks: %s\n", state, target, strings.Join(st.Networks, ", "))
	if len(st.Allow) == 0 {
		fmt.Fprintln(e.stdout, "allowed peers: none")
		return nil
	}
	tw := newTable(e.stdout)
	fmt.Fprintln(tw, "ALLOWED PEER\tID\tSTATE")
	for _, a := range st.Allow {
		name, state := a.Name, "active"
		if name == "" {
			name = "-"
		}
		if !a.Valid {
			state = "removed or re-paired: allow again"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, a.ID, state)
	}
	return tw.Flush()
}
