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
)

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
	// Probe, when set, is run first by Uninstall: if it fails there is nothing
	// registered, which counts as already uninstalled.
	Probe []string
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
			Notes:    []string{"sugar-it will start at your next logon. It does not start now; if the desktop app is running the daemon, that continues until the app closes."},
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
			Probe:    []string{"reg", "query", runKey, "/v", runValue},
			Commands: [][]string{{"reg", "delete", runKey, "/v", runValue, "/f"}},
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
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
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

// UninstallWith is Uninstall with an explicit Runner.
func UninstallWith(ctx context.Context, r Runner, u Unit, out io.Writer) error {
	var errs []error
	if len(u.Probe) > 0 && r.Run(ctx, u.Probe, io.Discard) != nil {
		fmt.Fprintln(out, "sugar-it is not installed as a background service.")
		return nil
	}
	for _, c := range u.Commands {
		if err := r.Run(ctx, c, out); err != nil {
			errs = append(errs, err)
		}
	}
	if u.Path != "" {
		if err := os.Remove(u.Path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
