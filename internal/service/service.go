// Package service installs sugar-it as a per-user background service: a
// systemd user unit on Linux, a LaunchAgent on macOS, a logon task on Windows.
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
	taskName    = "sugar-it"
)

// Unit is a service definition: the file to write (none when Path is empty)
// and the commands that activate or remove it.
type Unit struct {
	Path, Content string
	Commands      [][]string
}

// Plan builds the install plan for goos. exe is the sugar-it binary, home the
// user's home directory, configDir the --config-dir to pass ("" for the default).
// daemon is always the first argument: that is how the desktop binary, which
// the service runs when there is no sugar-it beside it, knows to start no window.
func Plan(goos, exe, home, configDir string) (Unit, error) {
	if exe == "" {
		return Unit{}, errors.New("no executable path")
	}
	switch goos {
	case "linux":
		p := path.Join(home, ".config", "systemd", "user", unitName)
		args := `"` + systemdEscape(exe) + `" daemon`
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
		argv := []string{exe, "daemon"}
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
		tr := `"` + exe + `" daemon`
		if configDir != "" {
			tr += ` --config-dir "` + configDir + `"`
		}
		return Unit{Commands: [][]string{
			{"schtasks", "/Create", "/SC", "ONLOGON", "/TN", taskName, "/TR", tr, "/RL", "LIMITED", "/F"},
			{"schtasks", "/Run", "/TN", taskName},
		}}, nil
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
		return Unit{Commands: [][]string{{"schtasks", "/End", "/TN", taskName}, {"schtasks", "/Delete", "/TN", taskName, "/F"}}}, nil
	}
	return Unit{}, fmt.Errorf("services are not supported on %s", goos)
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
