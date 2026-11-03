// Package mount builds and runs the OS command that mounts the WebDAV view of
// the mesh. Plans are pure data; Run executes them. The control token is never
// part of Argv, a note or an error: Linux gets it on stdin, macOS asks the user
// to type it into Finder's prompt, and Windows gets it as an argument of the
// WNetUseConnectionW call (see Plan.Windows), never on a command line.
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
	// Windows, when set, replaces Argv: connect with the WNet API (RunMount).
	Windows *WinConnect
}

// WinConnect describes the Windows connection. The password is the control
// token; it is given to RunMount, not stored here.
type WinConnect struct {
	RemoteName string // \\host@port\DavWWWRoot
	User       string
	Drive      string // "" lets Windows pick a free letter
}

// BasicAuthBlockedError means Windows' WebClient refuses HTTP Basic, so the
// mount cannot work until an administrator changes the setting.
type BasicAuthBlockedError struct {
	Level    uint64   // the BasicAuthLevel found
	Commands []string // to run in an administrator prompt
}

func (e *BasicAuthBlockedError) Error() string {
	return fmt.Sprintf("Windows blocks HTTP Basic authentication for WebDAV (BasicAuthLevel=%d, needs 2). "+
		"Run these in an administrator prompt, then mount again:\n  %s", e.Level, strings.Join(e.Commands, "\n  "))
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
			"  "+basicAuthCmds[0],
			"  "+basicAuthCmds[1])
	case w.BasicAuthLevel < 2:
		return Plan{}, &BasicAuthBlockedError{Level: w.BasicAuthLevel, Commands: append([]string(nil), basicAuthCmds[:]...)}
	}
	if w.Known && w.FileSizeLimit < maxFileLimit {
		notes = append(notes, fmt.Sprintf("Windows limits WebDAV files to %d bytes (FileSizeLimitInBytes). Larger files fail in Explorer; raise it (maximum 4294967295) in an administrator prompt, or use the sugar-it app for big files:", w.FileSizeLimit),
			"  reg add "+webClientKey+" /v FileSizeLimitInBytes /t REG_DWORD /d 4294967295 /f")
	}
	h, p, err := net.SplitHostPort(hp)
	if err != nil || strings.HasPrefix(h, "[") || strings.Contains(h, ":") {
		return Plan{}, fmt.Errorf("Windows cannot mount WebDAV at %q: use a host name or IPv4 address", hp)
	}
	notes = append(notes, "Connecting with user "+User+" and the control token as the password.")
	return Plan{
		Notes:   notes,
		Windows: &WinConnect{RemoteName: `\\` + h + "@" + p + `\DavWWWRoot`, User: User, Drive: strings.ToUpper(drive)},
	}, nil
}

var basicAuthCmds = [2]string{
	"reg add " + webClientKey + " /v BasicAuthLevel /t REG_DWORD /d 2 /f",
	"net stop webclient && net start webclient",
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
		return Plan{Argv: []string{"net", "use", drive, "/delete", "/y"}}, nil
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

// ConnectArgs are the arguments of one Windows connection. Password is the
// control token; it exists only as this function argument.
type ConnectArgs struct {
	RemoteName, User, Password, Drive string
}

// Connector makes the Windows connection and returns the drive it is on.
// DefaultConnector calls WNetUseConnectionW; tests inject a fake.
type Connector func(ConnectArgs) (drive string, err error)

// Connect runs a Windows plan through c. The token never reaches the error.
func Connect(c Connector, p Plan, token string) (string, error) {
	if p.Windows == nil {
		return "", errors.New("not a Windows connection plan")
	}
	drive, err := c(ConnectArgs{RemoteName: p.Windows.RemoteName, User: p.Windows.User, Password: token, Drive: p.Windows.Drive})
	if err != nil {
		msg := err.Error()
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "***")
		}
		return "", errors.New(msg)
	}
	return drive, nil
}

// RunMount prints the plan's notes and mounts it: a Windows plan through c, any
// other plan through r. It returns the drive Windows assigned, "" elsewhere.
func RunMount(ctx context.Context, r Runner, c Connector, p Plan, token string, out io.Writer) (string, error) {
	if p.Windows == nil {
		return "", RunWith(ctx, r, p, out)
	}
	for _, n := range p.Notes {
		fmt.Fprintln(out, n)
	}
	drive, err := Connect(c, p, token)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(out, "Mounted on "+drive)
	return drive, nil
}

// WinError turns a Windows system error code from the connection into a
// message that says what to do.
func WinError(code uint32) error {
	var msg string
	switch code {
	case 85:
		msg = "the drive letter is already in use: choose another letter, or disconnect it first (sugar-it unmount --drive Z:)"
	case 5, 86, 1326:
		msg = "Windows rejected the credentials (user " + User + " with the control token): check that the daemon is running with this config directory and that BasicAuthLevel is 2"
	case 1244:
		msg = "this session has no logon session that may connect to a network drive (for example over SSH): mount from the desktop session instead"
	case 53, 67, 1203, 1222, 1058, 1068, 1075:
		msg = "Windows could not reach the WebDAV server: the WebClient service is probably not running or cannot start (check that it is not disabled, then run net start webclient in an administrator prompt) and the sugar-it daemon must be running"
	case 259:
		msg = "no free drive letter is left: disconnect one first"
	case 1219:
		msg = "Windows already has a connection to this server with different credentials: disconnect it first (net use /delete)"
	default:
		msg = "Windows could not connect"
	}
	return fmt.Errorf("%s (system error %d)", msg, code)
}
