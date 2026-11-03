// Package cli is the sugar-it command line: a client of the Control API plus
// the daemon command.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
	"sugarintegration/internal/mount"
	"sugarintegration/internal/service"
	"sugarintegration/internal/vfs"
)

// Version is the release version. Release builds set it with
// -ldflags "-X sugarintegration/internal/cli.Version=...".
var Version = "dev"

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// exitError carries the exit code of a failed command.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}
func (e *exitError) Unwrap() error { return e.err }

func usageErr(err error) error { return &exitError{exitUsage, err} }

func usagef(format string, args ...any) error { return usageErr(fmt.Errorf(format, args...)) }

// env is the per-Execute state; nothing about a run lives in package variables.
type env struct {
	stdout, stderr io.Writer
	configDir      string
	json           bool
	mountRunner    mount.Runner
	mountConnect   mount.Connector // nil: the real Windows API
	svcRunner      service.Runner
}

// client connects to the running daemon of the config directory.
func (e *env) client() (*control.Client, error) {
	c, err := control.Discover(e.configDir)
	if err != nil {
		if errors.Is(err, control.ErrUnavailable) {
			return nil, fmt.Errorf("no sugar-it daemon is running for %s; start one with `sugar-it daemon`", e.dir())
		}
		return nil, err
	}
	return c, nil
}

func (e *env) dir() string {
	if e.configDir != "" {
		return e.configDir
	}
	d, err := config.DefaultDir()
	if err != nil {
		return "the default config directory"
	}
	return d
}

// run adapts a command body: runtime errors exit 1, usage errors keep their code.
func (e *env) run(f func(ctx context.Context, c *control.Client, args []string) error) func(*cobra.Command, []string) error {
	return plain(func(cmd *cobra.Command, args []string) error {
		c, err := e.client()
		if err != nil {
			return err
		}
		return f(cmd.Context(), c, args)
	})
}

// plain makes every error that is not already an exitError a failure (exit 1).
func plain(f func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := f(cmd, args)
		var ee *exitError
		if err == nil || errors.As(err, &ee) {
			return err
		}
		return &exitError{exitFailure, err}
	}
}

// exitWith ends the command with code after the command printed its own report.
func exitWith(code int) error { return &exitError{code: code} }

// Execute runs sugar-it with args (without the program name) and returns the
// exit code. It builds a fresh command tree on every call.
func Execute(args []string, stdout, stderr io.Writer) int {
	return execute(&env{stdout: stdout, stderr: stderr, mountRunner: mount.ExecRunner{}, svcRunner: service.ExecRunner{}}, args)
}

func execute(e *env, args []string) int {
	stdout, stderr := e.stdout, e.stderr
	root := newRoot(e)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	var ee *exitError
	code := exitUsage // cobra's own errors: unknown command or flag, wrong argument count
	if errors.As(err, &ee) {
		code = ee.code
	}
	if !(ee != nil && ee.err == nil) {
		fmt.Fprintf(stderr, "sugar-it: %s\n", errText(err))
	}
	return code
}

// errText is the message to show: vfs errors read "code: message".
func errText(err error) string {
	var ve *vfs.Error
	if errors.As(err, &ve) {
		return ve.Error()
	}
	return err.Error()
}

func newRoot(e *env) *cobra.Command {
	control.Version = Version // what `status` reports
	root := &cobra.Command{
		Version:       Version,
		Use:           "sugar-it",
		Short:         "Share folders between your computers",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE:          group,
	}
	root.PersistentFlags().StringVar(&e.configDir, "config-dir", "", "config directory (default: the user config directory)")
	root.PersistentFlags().BoolVar(&e.json, "json", false, "print JSON (query commands)")
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr(err) })
	root.AddCommand(e.daemonCmd(), e.stopCmd(), e.statusCmd(), e.inviteCmd(), e.peerCmd(), e.shareCmd(),
		e.lsCmd(), e.cpCmd("cp", "copy"), e.cpCmd("mv", "move"), e.rmCmd(), e.mkdirCmd(), e.renameCmd(), e.jobsCmd(), e.mountCmd(), e.unmountCmd(), e.serviceCmd())
	return root
}

func (e *env) daemonCmd() *cobra.Command {
	var o daemonFlags
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the sugar-it daemon in the background (--foreground: in this terminal)",
		Long: "Run the sugar-it daemon detached from this terminal; the command returns once it answers.\n" +
			"Its output goes to daemon.log in the config directory; stop it with `sugar-it stop`.\n" +
			"With --foreground the daemon runs in this process until Ctrl-C or SIGTERM,\n" +
			"which is what systemd and launchd want.",
		Args: cobra.NoArgs,
		RunE: plain(func(cmd *cobra.Command, _ []string) error {
			if !o.foreground {
				return e.startBackground(cmd.Context(), o)
			}
			return e.runForeground(cmd.Context(), o)
		}),
	}
	cmd.Flags().BoolVar(&o.foreground, "foreground", false, "run in this process instead of detaching")
	cmd.Flags().StringVar(&o.listen, "listen", "", "Peer API address (default: configured, :7443)")
	cmd.Flags().StringVar(&o.control, "control", "", "Control API address (default: configured, 127.0.0.1:7444)")
	cmd.Flags().StringVar(&o.dav, "dav", "", "WebDAV address (default: configured, 127.0.0.1:7445)")
	cmd.Flags().StringArrayVar(&o.advertise, "advertise", nil, "address other hosts should dial, host:port (repeatable; saved in the config)")
	return cmd
}

// runForeground is the daemon in this process. ctx ends on SIGINT and SIGTERM;
// the daemon also ends when a listener fails or `sugar-it stop` asks it to.
func (e *env) runForeground(ctx context.Context, o daemonFlags) error {
	d, err := daemon.Start(ctx, daemon.Options{ConfigDir: e.configDir, ListenAddr: o.listen, ControlAddr: o.control, DAVAddr: o.dav, AdvertiseAddrs: o.advertise})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stderr, "sugar-it daemon running: peer %s, control %s, webdav %s\n", d.PeerAddr(), d.ControlAddr(), d.DAVAddr())
	var failure error
	select {
	case <-ctx.Done():
	case <-d.Done():
	case failure = <-d.Failed():
	}
	sctx, cancel := context.WithTimeout(context.Background(), daemon.Grace+5*time.Second)
	defer cancel()
	if err := d.Shutdown(sctx); err != nil {
		return err
	}
	if err := d.Wait(); err != nil {
		return err
	}
	return failure
}

// group is the body of a command that only holds subcommands: an unknown word
// is a usage error, none at all shows the help.
func group(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("unknown command %q for %q", args[0], cmd.CommandPath())
	}
	return cmd.Help()
}
