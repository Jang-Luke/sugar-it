// Package service installs sugar-it as a per-user background service: a
// systemd user unit on Linux, a LaunchAgent on macOS, a per-user Run key value on Windows (a scheduled task needs elevation).
// Everything runs with the user's own rights. Plan and UninstallPlan are pure;
// Install and Uninstall execute them.
package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"sugarintegration/internal/vfs"
	"sugarintegration/internal/winout"
)

const legacyTask = "sugar-it" // the scheduled task v0.1.0 created on Windows

const (
	unitName    = "sugar-it.service"
	launchLabel = "io.sugar-it.daemon"
	runValue    = "sugar-it"
	runKey      = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
)

// Unit is a service definition: the file to write (none when Path is empty)
// and the commands that activate or remove it.
type Unit struct {
	Path, Content string
	Commands      [][]string
	Notes         []string // printed after a successful install
	// Probe, when set, is run first by Uninstall; Commands run only if it
	// succeeds. When it fails, ProbeKey tells "the value is not there" (the
	// key can be read) from "the registry cannot be read" (reported as an
	// error), without depending on the language of reg's message.
	Probe, ProbeKey []string
	// LegacyProbe and LegacyDelete remove the v0.1.0 scheduled task: when the
	// probe finds it, LegacyDelete runs; if that fails (deleting a task made
	// with elevation needs an administrator) the error says what to run.
	LegacyProbe, LegacyDelete []string
}

// Plan builds the install plan for goos. exe is the sugar-it binary, home the
// user's home directory, configDir the --config-dir to pass ("" for the default).
// daemon is always the first argument: that is how the desktop binary, which
// the service runs when there is no sugar-it beside it, knows to start no window.
// systemd and launchd supervise the process they start, so on Linux and macOS
// it is `daemon --foreground`. The Windows Run key value is plain `daemon`,
// which detaches, so that sugar-it.exe leaves no console window open.
func Plan(goos, exe, home, configDir string) (Unit, error) {
	if exe == "" {
		return Unit{}, errors.New("no executable path")
	}
	switch goos {
	case "linux":
		p := path.Join(home, ".config", "systemd", "user", unitName)
		args := `"` + systemdEscape(exe) + `" daemon --foreground`
		if configDir != "" {
			args += ` --config-dir "` + systemdEscape(configDir) + `"`
		}
		content := "[Unit]\nDescription=sugar-it shared folder mesh\nAfter=network-online.target\n\n" +
			"[Service]\nExecStart=" + args + "\nRestart=on-failure\nRestartSec=5\n\n" +
			"[Install]\nWantedBy=default.target\n"
		return Unit{Path: p, Content: content, Commands: [][]string{
			{"loginctl", "enable-linger"},
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", "--now", unitName},
		}}, nil
	case "darwin":
		p := darwinPath(home)
		argv := []string{exe, "daemon", "--foreground"}
		if configDir != "" {
			argv = append(argv, "--config-dir", configDir)
		}
		var b bytes.Buffer
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
		b.WriteString("<plist version=\"1.0\">\n<dict>\n\t<key>Label</key>\n\t<string>" + launchLabel + "</string>\n\t<key>ProgramArguments</key>\n\t<array>\n")
		for _, a := range argv {
			b.WriteString("\t\t<string>")
			xml.EscapeText(&b, []byte(a))
			b.WriteString("</string>\n")
		}
		b.WriteString("\t</array>\n\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<true/>\n</dict>\n</plist>\n")
		return Unit{Path: p, Content: b.String(), Commands: [][]string{{"launchctl", "load", "-w", p}}}, nil
	case "windows":
		// The value is a command line that Windows parses at logon with the C
		// runtime rules, so a trailing backslash before the closing quote is
		// doubled. os/exec escapes the whole argument again for reg itself.
		v := `"` + exe + `" daemon`
		if configDir != "" {
			v += ` --config-dir "` + configDir + strings.Repeat(`\`, trailingBackslashes(configDir)) + `"`
		}
		return Unit{
			Commands: [][]string{{"reg", "add", runKey, "/v", runValue, "/t", "REG_SZ", "/d", v, "/f"}},
			Notes:    []string{"sugar-it will start at your next logon. It does not start now; if the desktop app is running the daemon, that continues until the app quits."},
		}, nil
	}
	return Unit{}, fmt.Errorf("services are not supported on %s", goos)
}

// UninstallPlan builds the removal plan: Path is the file to delete.
func UninstallPlan(goos, home string) (Unit, error) {
	switch goos {
	case "linux":
		return Unit{Path: path.Join(home, ".config", "systemd", "user", unitName), Commands: [][]string{
			{"systemctl", "--user", "disable", "--now", unitName},
		}}, nil
	case "darwin":
		p := darwinPath(home)
		return Unit{Path: p, Commands: [][]string{{"launchctl", "unload", "-w", p}}}, nil
	case "windows":
		return Unit{
			Probe:        []string{"reg", "query", runKey, "/v", runValue},
			ProbeKey:     []string{"reg", "query", runKey},
			Commands:     [][]string{{"reg", "delete", runKey, "/v", runValue, "/f"}},
			LegacyProbe:  []string{"schtasks", "/Query", "/TN", legacyTask},
			LegacyDelete: []string{"schtasks", "/Delete", "/TN", legacyTask, "/F"},
		}, nil
	}
	return Unit{}, fmt.Errorf("services are not supported on %s", goos)
}

func trailingBackslashes(s string) int {
	n := 0
	for strings.HasSuffix(s[:len(s)-n], `\`) {
		n++
	}
	return n
}

func darwinPath(home string) string {
	return path.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

// systemdEscape escapes s for use inside a double-quoted ExecStart word:
// backslash and quote, and the % and $ that systemd expands even in quotes.
func systemdEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s)
}

// Runner executes one command.
type Runner interface {
	Run(ctx context.Context, argv []string, out io.Writer) error
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, argv []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Console programs write in the console's code page, not UTF-8.
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	io.WriteString(out, winout.Decode(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// Install writes the unit file and runs its commands.
func Install(ctx context.Context, u Unit, out io.Writer) error {
	return InstallWith(ctx, ExecRunner{}, u, out)
}

// InstallWith is Install with an explicit Runner.
func InstallWith(ctx context.Context, r Runner, u Unit, out io.Writer) error {
	if u.Path != "" {
		if err := os.MkdirAll(filepath.Dir(u.Path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(u.Path, []byte(u.Content), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s\n", u.Path)
	}
	for _, c := range u.Commands {
		if err := r.Run(ctx, c, out); err != nil {
			return err
		}
	}
	for _, n := range u.Notes {
		fmt.Fprintln(out, n)
	}
	return nil
}

// Uninstall runs the removal commands and deletes the unit file. Every step is
// attempted: a service that is not running must not keep the file around.
func Uninstall(ctx context.Context, u Unit, out io.Writer) error {
	return UninstallWith(ctx, ExecRunner{}, u, out)
}

// UninstallWith is Uninstall with an explicit Runner. On Windows it reports
// what it removed: the Run value, the v0.1.0 scheduled task, or that neither
// was there. It does not stop a running daemon; see StopDaemon.
func UninstallWith(ctx context.Context, r Runner, u Unit, out io.Writer) error {
	var errs []error
	found := true
	if len(u.Probe) > 0 {
		if r.Run(ctx, u.Probe, io.Discard) != nil {
			found = false
			if len(u.ProbeKey) > 0 && r.Run(ctx, u.ProbeKey, io.Discard) != nil {
				errs = append(errs, fmt.Errorf("cannot read the registry to see whether the logon entry exists (%s)", strings.Join(u.ProbeKey, " ")))
			}
		}
	}
	if found {
		var cmdErrs []error
		for _, c := range u.Commands {
			if err := r.Run(ctx, c, out); err != nil {
				cmdErrs = append(cmdErrs, err)
			}
		}
		errs = append(errs, cmdErrs...)
		if len(u.Probe) > 0 && len(cmdErrs) == 0 {
			fmt.Fprintln(out, "Removed the logon entry (Run value "+runValue+"): sugar-it no longer starts at logon.")
		}
	}
	legacy := false
	if len(u.LegacyProbe) > 0 && r.Run(ctx, u.LegacyProbe, io.Discard) == nil {
		legacy = true
		if err := r.Run(ctx, u.LegacyDelete, out); err != nil {
			errs = append(errs, fmt.Errorf("the scheduled task %q from sugar-it v0.1.0 is still there: deleting it needs an administrator. Run in an administrator prompt: %s",
				legacyTask, strings.Join(u.LegacyDelete, " ")))
		} else {
			fmt.Fprintf(out, "Removed the scheduled task %q that sugar-it v0.1.0 created.\n", legacyTask)
		}
	}
	if len(u.Probe) > 0 && !found && !legacy && len(errs) == 0 {
		fmt.Fprintln(out, "sugar-it is not installed as a background service.")
	}
	if u.Path != "" {
		if err := os.Remove(u.Path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Daemon is what StopDaemon needs of a running daemon: *control.Client is one.
type Daemon interface {
	Shutdown(ctx context.Context) error
}

// StopDaemon is the second half of a Windows uninstall, which removes the
// logon entry but cannot stop a process: d is the daemon that answers for the
// config directory, nil if none does. It asks that daemon to stop and says
// which happened. A daemon inside the desktop app refuses; it stops when the
// app closes. wait, if set, runs after the request is accepted, until the
// daemon is gone.
func StopDaemon(ctx context.Context, d Daemon, wait func(context.Context) error, out io.Writer) error {
	if d == nil {
		fmt.Fprintln(out, "No daemon is running for this config directory, so there is nothing to stop.")
		return nil
	}
	if err := d.Shutdown(ctx); err != nil {
		if vfs.CodeOf(err) == vfs.CodeInvalidRequest {
			fmt.Fprintln(out, "The daemon runs inside the desktop app, so it keeps running and stops when the app quits.")
			return nil
		}
		return fmt.Errorf("could not stop the running daemon: %w", err)
	}
	if wait != nil {
		if err := wait(ctx); err != nil {
			return err
		}
		fmt.Fprintln(out, "Stopped the running daemon.")
		return nil
	}
	fmt.Fprintln(out, "Asked the running daemon to stop.")
	return nil
}
