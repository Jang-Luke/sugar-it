// Package mount builds and runs the OS command that mounts the WebDAV view of
// the mesh. Plans are pure data; Run executes them. The control token is never
// part of Argv, a note or an error: Linux gets it on stdin, macOS and Windows
// ask the user to type it into the OS password prompt.
package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// User is the WebDAV user name; the password is the control token.
const User = "sugar-it"

// Plan is one command to run.
type Plan struct {
	Argv  []string
	Stdin string   // fed to the command; the token for Linux, empty elsewhere
	Notes []string // shown to the user before the command runs
}

// WinLimits are the WebClient registry values that matter to a mount.
type WinLimits struct {
	BasicAuthLevel, FileSizeLimit uint64
	Known                         bool // false when they could not be read
}

const (
	webClientKey = `HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters`
	maxFileLimit = 4294967295 // the registry value is a 32-bit DWORD
)

var (
	driveRe = regexp.MustCompile(`^[A-Za-z]:$`)
	hostRe  = regexp.MustCompile(`^[A-Za-z0-9.\-]+$|^\[[0-9A-Fa-f:.]+\]$`)
	portRe  = regexp.MustCompile(`^[0-9]{1,5}$`)
)

func checkDrive(drive string) error {
	if drive != "" && !driveRe.MatchString(drive) {
		return fmt.Errorf("invalid drive %q: use a drive letter and a colon, such as Z:", drive)
	}
	return nil
}

// davURL validates davAddr (host:port) and returns host:port normalised.
func hostPort(davAddr string) (string, error) {
	h, p, err := net.SplitHostPort(davAddr)
	if err != nil || h == "" || p == "" {
		return "", fmt.Errorf("invalid WebDAV address %q", davAddr)
	}
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if !hostRe.MatchString(h) || !portRe.MatchString(p) {
		return "", fmt.Errorf("invalid WebDAV address %q", davAddr)
	}
	return h + ":" + p, nil
}

// MountPlan returns the command that mounts the WebDAV server at davAddr on
// goos. drive (like "Z:") is used on Windows only; "" picks a free letter.
func MountPlan(goos, davAddr, token, drive string, w WinLimits) (Plan, error) {
	if err := checkDrive(drive); err != nil {
		return Plan{}, err
	}
	hp, err := hostPort(davAddr)
	if err != nil {
		return Plan{}, err
	}
	switch goos {
	case "linux":
		return Plan{
			Argv:  []string{"gio", "mount", "dav://" + User + "@" + hp + "/"},
			Stdin: token + "\n",
			Notes: []string{"Mounting with gio; the folders appear in your file manager under Network."},
		}, nil
	case "darwin":
		return Plan{
			Argv: []string{"osascript", "-e", `mount volume "http://` + hp + `/"`},
			Notes: []string{
				"Finder will ask for credentials. Choose \"Registered User\" and enter:",
				"  Name:     " + User,
				"  Password: the token printed below",
			},
		}, nil
	case "windows":
		return windowsPlan(hp, drive, w)
	}
	return Plan{}, fmt.Errorf("mounting is not supported on %s", goos)
}

func windowsPlan(hp, drive string, w WinLimits) (Plan, error) {
	var notes []string
	switch {
	case !w.Known:
		notes = append(notes, "Could not read the WebClient settings. If the mount is refused, allow Basic authentication (run in an administrator prompt, then restart the WebClient service):",
			"  reg add "+webClientKey+" /v BasicAuthLevel /t REG_DWORD /d 2 /f",
			"  net stop webclient && net start webclient")
	case w.BasicAuthLevel < 2:
		return Plan{}, fmt.Errorf("Windows blocks HTTP Basic authentication for WebDAV (BasicAuthLevel=%d, needs 2). "+
			"Run these in an administrator prompt, then mount again:\n"+
			"  reg add %s /v BasicAuthLevel /t REG_DWORD /d 2 /f\n"+
			"  net stop webclient && net start webclient", w.BasicAuthLevel, webClientKey)
	}
	if w.Known && w.FileSizeLimit < maxFileLimit {
		notes = append(notes, fmt.Sprintf("Windows limits WebDAV files to %d bytes (FileSizeLimitInBytes). Larger files fail in Explorer; raise it (maximum 4294967295) in an administrator prompt, or use the sugar-it app for big files:", w.FileSizeLimit),
			"  reg add "+webClientKey+" /v FileSizeLimitInBytes /t REG_DWORD /d 4294967295 /f")
	}
	d := drive
	if d == "" {
		d = "*"
	}
	notes = append(notes, "Windows will ask for the password. User: "+User+"; Password: the token printed below")
	return Plan{Argv: []string{"net", "use", d, "http://" + hp + "/", "/user:" + User, "*"}, Notes: notes}, nil
}

// UnmountPlan returns the command that undoes MountPlan.
func UnmountPlan(goos, davAddr, drive string) (Plan, error) {
	if err := checkDrive(drive); err != nil {
		return Plan{}, err
	}
	hp, err := hostPort(davAddr)
	if err != nil {
		return Plan{}, err
	}
	switch goos {
	case "linux":
		return Plan{Argv: []string{"gio", "mount", "-u", "dav://" + User + "@" + hp + "/"}}, nil
	case "darwin":
		h, _, _ := net.SplitHostPort(davAddr)
		return Plan{Argv: []string{"diskutil", "unmount", "/Volumes/" + h}, Notes: []string{"If the volume has another name, eject it in Finder."}}, nil
	case "windows":
		if drive == "" {
			return Plan{}, errors.New("give the drive to disconnect with --drive, such as --drive Z:")
		}
		return Plan{Argv: []string{"net", "use", drive, "/delete"}}, nil
	}
	return Plan{}, fmt.Errorf("mounting is not supported on %s", goos)
}

// Runner executes one command. Tests inject a fake; ExecRunner is the real one.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin string, out io.Writer) error
}

// ExecRunner runs commands with os/exec, wired to the user's terminal where a
// prompt needs it.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, argv []string, stdin string, out io.Writer) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = out, out
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	} else {
		cmd.Stdin = os.Stdin // the OS prompts for the password
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", argv[0], err)
	}
	return nil
}

// Run prints the plan's notes to out and executes it with the real runner.
func Run(ctx context.Context, p Plan, out io.Writer) error { return RunWith(ctx, ExecRunner{}, p, out) }

// RunWith is Run with an explicit Runner.
func RunWith(ctx context.Context, r Runner, p Plan, out io.Writer) error {
	for _, n := range p.Notes {
		fmt.Fprintln(out, n)
	}
	return r.Run(ctx, p.Argv, p.Stdin, out)
}
