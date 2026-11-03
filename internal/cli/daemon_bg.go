package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
)

const (
	logName       = "daemon.log"
	logMaxSize    = 10 << 20 // rotated at the next start when larger
	startWait     = 15 * time.Second
	logTailLines  = 20
	logTailMaxLen = 8 << 10
)

// daemonFlags are the flags of `sugar-it daemon`.
type daemonFlags struct {
	foreground           bool
	listen, control, dav string
	advertise            []string
}

// childArgs is the command line of the detached daemon: daemon is the first
// argument, so the desktop binary (see desktop runDaemon) takes it for the CLI.
func childArgs(configDir string, o daemonFlags) []string {
	args := []string{"daemon", "--foreground"}
	if configDir != "" {
		args = append(args, "--config-dir", configDir)
	}
	for _, f := range []struct{ name, val string }{{"listen", o.listen}, {"control", o.control}, {"dav", o.dav}} {
		if f.val != "" {
			args = append(args, "--"+f.name, f.val)
		}
	}
	for _, a := range o.advertise {
		args = append(args, "--advertise", a)
	}
	return args
}

// rotateLog renames path to path+".1", replacing an older one, if it is larger
// than max bytes. A missing file is fine.
func rotateLog(path string, max int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.Size() <= max {
		return nil
	}
	return os.Rename(path, path+".1")
}

// startBackground re-executes this program as `daemon --foreground`, detached
// from the terminal, and returns once the child answers on the Control API.
func (e *env) startBackground(ctx context.Context, o daemonFlags) error {
	dir := e.configDir
	if dir == "" {
		d, err := config.DefaultDir()
		if err != nil {
			return err
		}
		dir = d
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// A daemon that answers is left alone: nothing is rotated, opened or started.
	if _, err := control.Discover(dir); err == nil {
		return fmt.Errorf("another sugar-it daemon is already running with the config directory %s", dir)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(dir, logName)
	if err := rotateLog(logPath, logMaxSize); err != nil {
		return fmt.Errorf("rotate %s: %w", logPath, err)
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	offset, _ := lf.Seek(0, io.SeekEnd)

	newChild := func() *exec.Cmd {
		c := exec.Command(exe, childArgs(dir, o)...)
		c.Stdin = nil // the null device
		c.Stdout, c.Stderr = lf, lf
		return c
	}
	cmd := newChild()
	detach(cmd, true)
	if err := cmd.Start(); err != nil {
		// A job object can forbid breakaway on Windows; start inside it instead.
		cmd = newChild()
		detach(cmd, false)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start the daemon: %w", err)
		}
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	fail := func(why string) error {
		fmt.Fprintf(e.stderr, "sugar-it: %s; the end of %s:\n%s", why, logPath, logTail(logPath, offset))
		return exitWith(exitFailure)
	}
	timeout := time.After(startWait)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
			return fail("the daemon exited before it was ready")
		case <-ctx.Done():
			killChild(cmd, exited)
			return fail("interrupted before the daemon was ready; it was stopped")
		case <-timeout:
			killChild(cmd, exited)
			return fail(fmt.Sprintf("the daemon did not answer within %v; it was stopped", startWait))
		case <-tick.C:
		}
		rt, err := control.ReadRuntime(dir)
		if err != nil || rt.PID != cmd.Process.Pid {
			continue
		}
		if _, err := control.Discover(dir); err != nil {
			continue
		}
		stop := "sugar-it stop"
		if e.configDir != "" {
			stop += " --config-dir " + quoteArg(dir)
		}
		fmt.Fprintf(e.stdout, "sugar-it daemon started in the background: peer %s, control %s, webdav %s; log %s; stop with `%s`\n",
			rt.PeerAddr, rt.ControlAddr, rt.DAVAddr, logPath, stop)
		return nil
	}
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"'") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// killChild stops a daemon that never became ready and waits for it to go.
func killChild(cmd *exec.Cmd, exited <-chan struct{}) {
	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
}

// logTail is the last lines of the log written since offset.
func logTail(path string, offset int64) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(cannot read the log: %v)\n", err)
	}
	if offset > 0 && offset <= int64(len(b)) {
		b = b[offset:]
	}
	if len(b) > logTailMaxLen {
		b = b[len(b)-logTailMaxLen:]
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > logTailLines {
		lines = lines[len(lines)-logTailLines:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return "(the daemon wrote nothing)\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func (e *env) stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running sugar-it daemon",
		Args:  cobra.NoArgs,
		RunE: plain(func(cmd *cobra.Command, _ []string) error {
			c, err := e.client()
			if err != nil {
				return err
			}
			if err := c.Shutdown(cmd.Context()); err != nil {
				return err
			}
			if err := waitStopped(cmd.Context(), c, e.configDirOrDefault(), daemon.Grace+5*time.Second); err != nil {
				return err
			}
			fmt.Fprintln(e.stdout, "sugar-it daemon stopped")
			return nil
		}),
	}
}

func (e *env) configDirOrDefault() string {
	if e.configDir != "" {
		return e.configDir
	}
	d, _ := config.DefaultDir()
	return d
}

// waitStopped waits until the Control API no longer answers and the daemon has
// removed its runtime.json, which it does last but for its lock.
func waitStopped(ctx context.Context, c *control.Client, dir string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	answering := true
	for {
		if answering {
			qctx, cancel := context.WithTimeout(ctx, time.Second)
			_, err := c.Status(qctx)
			cancel()
			answering = !errors.Is(err, control.ErrUnavailable)
		}
		if !answering {
			if _, err := control.ReadRuntime(dir); err != nil {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon did not stop within %v", limit)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
