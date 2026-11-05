package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/detach"
	"sugarintegration/internal/fsutil"
)

const (
	// HelperCommand is the first argument that makes the desktop program the helper.
	HelperCommand = "update-helper"
	// HelperExe is the name of the helper's copy of the program.
	HelperExe = "sugar-it-update-helper.exe"
	// TempPrefix starts the name of every update's temporary directory.
	TempPrefix = "sugar-it-update-"
	// StatusFile, in the config directory, tells the old app why an update failed.
	StatusFile = "update-status.json"
	// MarkerFile, in the temporary directory, tells the app that the helper is running.
	MarkerFile = "helper-started"

	// HelperWait is how long the helper waits for the app to exit.
	HelperWait = 60 * time.Second
	// MarkerWait is how long the app waits for the helper to show it is running. An
	// unsigned program may be scanned for some seconds before it runs.
	MarkerWait = 15 * time.Second
	// SweepAge is the age at which a left-over temporary directory is removed. The
	// helper cannot delete its own running copy, so one is always left behind.
	SweepAge = time.Hour

	daemonStartWait = 20 * time.Second // sugar-it daemon itself gives up after 15 seconds
)

// Why an update failed, in Status.Outcome. ko.ts has the text of each.
const (
	OutcomeDenied          = "update_denied"           // the UAC prompt was refused
	OutcomeInstallerFailed = "update_installer_failed" // the installer failed or could not start
	OutcomeTimeout         = "update_timeout"          // the app did not exit in time
	// The installer file is not what was verified, so it did not run.
	OutcomeInstallerChanged = "update_installer_changed"
	// The installer outlived the helper's wait and may still be running.
	OutcomeInstallerTimeout = "update_installer_timeout"
	// The update went through but the stopped daemon did not start again.
	OutcomeDaemonRestart = "update_daemon_restart_failed"
)

var (
	// ErrElevationDenied: the user refused the UAC prompt.
	ErrElevationDenied = errors.New("elevation was refused")
	// ErrWaitTimeout: the process did not exit in time.
	ErrWaitTimeout = errors.New("the process did not exit in time")
	// ErrInstallerChanged: the installer is gone or differs from what was verified.
	ErrInstallerChanged = errors.New("the installer is not the file that was verified")
	// ErrInstallerBusy: the handle held on the installer kept it from starting.
	ErrInstallerBusy = errors.New("the installer could not start while it was held open")
	// ErrHelperGone: the helper did not show that it is running; something removed it.
	ErrHelperGone = errors.New("the update helper did not start")
	// ErrUnsupported: this system has no helper.
	ErrUnsupported = errors.New("the update helper runs only on Windows")
)

var sha256RE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// HelperArgs is the helper's command line. ConfigDir is empty for the default.
type HelperArgs struct {
	PID            int
	Installer, App string
	SHA256         string // the installer's verified hash
	ConfigDir      string
	RestartDaemon  bool
}

// Argv is the helper's arguments, starting with HelperCommand.
func (a HelperArgs) Argv() []string {
	argv := []string{HelperCommand, "--pid", strconv.Itoa(a.PID), "--installer", a.Installer, "--sha256", a.SHA256, "--app", a.App}
	if a.ConfigDir != "" {
		argv = append(argv, "--config-dir", a.ConfigDir)
	}
	if a.RestartDaemon {
		argv = append(argv, "--restart-daemon")
	}
	return argv
}

// ParseHelperArgs parses what follows HelperCommand.
func ParseHelperArgs(args []string) (HelperArgs, error) {
	var a HelperArgs
	fs := flag.NewFlagSet(HelperCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(&a.PID, "pid", 0, "")
	fs.StringVar(&a.Installer, "installer", "", "")
	fs.StringVar(&a.SHA256, "sha256", "", "")
	fs.StringVar(&a.App, "app", "", "")
	fs.StringVar(&a.ConfigDir, "config-dir", "", "")
	fs.BoolVar(&a.RestartDaemon, "restart-daemon", false, "")
	if err := fs.Parse(args); err != nil {
		return HelperArgs{}, err
	}
	if fs.NArg() != 0 || a.PID <= 0 || a.Installer == "" || a.App == "" || !sha256RE.MatchString(a.SHA256) {
		return HelperArgs{}, errors.New("usage: update-helper --pid <pid> --installer <path> --sha256 <hex> --app <path> [--config-dir <dir>] [--restart-daemon]")
	}
	return a, nil
}

// InstallerParams is the silent installer's command line. Without /D the installer
// would use its default folder, and relaunching the app from another folder would
// start the old version again. NSIS wants /D last and unquoted, spaces included.
func InstallerParams(appDir string) string { return "/S /D=" + appDir }

// AppArgv is the relaunched app's arguments.
func AppArgv(configDir string) []string {
	if configDir == "" {
		return nil
	}
	return []string{"--config-dir", configDir}
}

// DaemonArgv is the arguments that start the app's program as a background daemon.
func DaemonArgv(configDir string) []string {
	return append([]string{"daemon"}, AppArgv(configDir)...)
}

// Status is what the helper leaves for the app it relaunches after a failure.
type Status struct {
	Outcome string
	Code    int    // the installer's exit code, if it ran
	Detail  string // an English error, shown as the detail
	// DaemonOff: the daemon stopped for the update did not start again.
	DaemonOff bool
}

// WriteStatus stores s in the config directory dir.
func WriteStatus(dir string, s Status) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(dir, StatusFile), b, 0o600)
}

// TakeStatus reads and removes the status file. Anything unexpected in it is dropped.
func TakeStatus(dir string) (Status, bool) {
	path := filepath.Join(dir, StatusFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return Status{}, false
	}
	os.Remove(path)
	var s Status
	if json.Unmarshal(b, &s) != nil {
		return Status{}, false
	}
	switch s.Outcome {
	case OutcomeDenied, OutcomeInstallerFailed, OutcomeTimeout,
		OutcomeInstallerChanged, OutcomeInstallerTimeout, OutcomeDaemonRestart:
		return s, true
	}
	return Status{}, false
}

// HelperOps are the helper's effects, so that RunHelper can be tested anywhere.
type HelperOps struct {
	SelfExe     func() (string, error)
	HashFile    func(path string) (string, error)
	Mark        func(dir string) error
	WaitExit    func(pid int, d time.Duration) error
	Hold        func(file, wantSHA256 string) (io.Closer, error)
	RunElevated func(file, params string) (exitCode int, err error)
	StartDaemon func(app, configDir string) error
	StartApp    func(app, configDir string) error
	WriteStatus func(Status) error
	RemoveAll   func(dir string)
}

// RunHelper replaces the app: it waits for it to exit, runs the installer, checks
// that the program changed, and starts the app again. It has no window, so a failure
// reaches the user through the status file, shown by the app.
func RunHelper(a HelperArgs, ops HelperOps) int {
	dir := filepath.Dir(a.Installer)
	if err := ops.Mark(dir); err != nil {
		fmt.Fprintf(os.Stderr, "sugar-it: mark the helper as started: %v\n", err)
	}
	s, appRuns := install(a, ops)
	// The daemon first: an app started first would host a daemon of its own and
	// the service would then be refused.
	if a.RestartDaemon {
		if err := ops.StartDaemon(a.App, a.ConfigDir); err != nil {
			s.DaemonOff = true
			if s.Outcome == "" {
				s.Outcome, s.Detail = OutcomeDaemonRestart, err.Error()
			}
		}
	}
	code := 0
	if s.Outcome != "" {
		code = 1
		// Before the app starts, so that it finds the status.
		if err := ops.WriteStatus(s); err != nil {
			fmt.Fprintf(os.Stderr, "sugar-it: write the update status: %v\n", err)
		}
	}
	// A running app holds the single-instance lock: a second one would only exit.
	if !appRuns {
		if err := ops.StartApp(a.App, a.ConfigDir); err != nil {
			fmt.Fprintf(os.Stderr, "sugar-it: restart the app: %v\n", err)
			code = 1
		}
	}
	if strings.HasPrefix(filepath.Base(dir), TempPrefix) {
		ops.RemoveAll(dir) // removes all but the running helper; see SweepTemp
	}
	return code
}

// install reports what went wrong, if anything, and whether the app is still running.
func install(a HelperArgs, ops HelperOps) (s Status, appRuns bool) {
	if err := ops.WaitExit(a.PID, HelperWait); err != nil {
		return Status{Outcome: OutcomeTimeout, Detail: err.Error()}, errors.Is(err, ErrWaitTimeout)
	}
	// Held from before the check until the installer has started, so that nothing can
	// change the file in between. The wait for the app and UAC come after the download.
	hold, err := ops.Hold(a.Installer, a.SHA256)
	if err != nil {
		return Status{Outcome: OutcomeInstallerChanged, Detail: err.Error()}, false
	}
	params := InstallerParams(filepath.Dir(a.App))
	code, err := ops.RunElevated(a.Installer, params)
	hold.Close()
	if errors.Is(err, ErrInstallerBusy) {
		// The hold kept it from starting, so the check is as late as it can be
		// without one. A process of this user could still swap the file in between.
		got, herr := ops.HashFile(a.Installer)
		if herr != nil || !strings.EqualFold(got, a.SHA256) {
			return Status{Outcome: OutcomeInstallerChanged, Detail: ErrInstallerChanged.Error()}, false
		}
		code, err = ops.RunElevated(a.Installer, params)
	}
	switch {
	case errors.Is(err, ErrElevationDenied):
		return Status{Outcome: OutcomeDenied}, false
	case errors.Is(err, ErrWaitTimeout):
		detail := "the program is unchanged"
		if unchanged, herr := programUnchanged(a, ops); herr == nil && !unchanged {
			detail = "the program was replaced, but the installer has not finished"
		}
		return Status{Outcome: OutcomeInstallerTimeout, Detail: "the installer is still running; " + detail}, false
	case err != nil:
		return Status{Outcome: OutcomeInstallerFailed, Detail: err.Error()}, false
	case code != 0:
		return Status{Outcome: OutcomeInstallerFailed, Code: code, Detail: fmt.Sprintf("the installer exited with %d", code)}, false
	}
	// Silent NSIS skips a locked file and still exits 0, so the exit code proves
	// nothing. The helper is a byte copy of the old program.
	unchanged, err := programUnchanged(a, ops)
	if err != nil {
		return Status{Outcome: OutcomeInstallerFailed, Detail: "cannot read the program after installing: " + err.Error()}, false
	}
	if unchanged {
		return Status{Outcome: OutcomeInstallerFailed, Detail: "the installer exited with 0 but left the program unchanged; something held the file open"}, false
	}
	return Status{}, false
}

// programUnchanged reports whether the app's file is still this helper's bytes.
func programUnchanged(a HelperArgs, ops HelperOps) (bool, error) {
	self, err := ops.SelfExe()
	if err != nil {
		return false, err
	}
	old, err := ops.HashFile(self)
	if err != nil {
		return false, err
	}
	cur, err := ops.HashFile(a.App)
	if err != nil {
		return false, err
	}
	return old == cur, nil
}

// HashFile is a file's SHA-256 in hex.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(f)
}

func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HoldVerified opens path with open, which should deny writers, and checks its hash
// through that handle. The caller keeps the handle until the file has been used.
func HoldVerified(open func(string) (*os.File, error), path, want string) (io.Closer, error) {
	f, err := open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInstallerChanged, err)
	}
	got, err := hashReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %v", ErrInstallerChanged, err)
	}
	if !strings.EqualFold(got, want) {
		f.Close()
		return nil, ErrInstallerChanged
	}
	return f, nil
}

// WriteMarker tells the app, waiting in WaitMarker, that the helper is running.
func WriteMarker(dir string) error {
	return os.WriteFile(filepath.Join(dir, MarkerFile), nil, 0o600)
}

// WaitMarker waits for the helper's marker. Without one the helper never ran, for
// instance because an antivirus removed it, and the app must not quit.
func WaitMarker(dir string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w within %v", ErrHelperGone, d)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// HelperMain is the update-helper command. It runs before anything of the GUI.
func HelperMain(args []string, stderr io.Writer) int {
	a, err := ParseHelperArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ops, err := platformOps()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	statusDir := a.ConfigDir
	if statusDir == "" {
		if statusDir, err = config.DefaultDir(); err != nil {
			fmt.Fprintln(stderr, err)
		}
	}
	ops.SelfExe = os.Executable
	ops.HashFile = HashFile
	ops.Mark = WriteMarker
	ops.StartDaemon = StartDaemon
	ops.StartApp = startApp
	ops.WriteStatus = func(s Status) error { return WriteStatus(statusDir, s) }
	ops.RemoveAll = func(dir string) { os.RemoveAll(dir) }
	return RunHelper(a, ops)
}

// StartDaemon starts app as the background daemon and waits until the starting
// process returns, which it does once the daemon answers or has failed.
func StartDaemon(app, configDir string) error {
	cmd, err := detach.Start(func() *exec.Cmd {
		c := exec.Command(app, DaemonArgv(configDir)...)
		c.Dir = filepath.Dir(app)
		return c
	})
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(daemonStartWait):
		return fmt.Errorf("the daemon did not answer within %v", daemonStartWait)
	}
}

func startApp(app, configDir string) error {
	cmd, err := detach.StartGUI(func() *exec.Cmd {
		c := exec.Command(app, AppArgv(configDir)...)
		c.Dir = filepath.Dir(app)
		return c
	})
	if err != nil {
		return err
	}
	return cmd.Process.Release()
}

// StartHelper starts the helper from its copy of the program, in dir, detached so
// that it outlives the app. Its directory is dir and not the app's, so the installer
// never finds the app's folder held open by it. abort stops it again.
func StartHelper(helperExe, dir string, a HelperArgs) (abort func(), err error) {
	cmd, err := detach.Start(func() *exec.Cmd {
		c := exec.Command(helperExe, a.Argv()...)
		c.Dir = dir
		return c
	})
	if err != nil {
		return nil, err
	}
	return func() { cmd.Process.Kill() }, nil
}

// SweepTemp removes earlier updates' directories under root, which are old enough
// that no helper is still using them. Failures do not matter.
func SweepTemp(root string, now time.Time) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), TempPrefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) < SweepAge {
			continue
		}
		os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// SameFile reports whether two paths are one file. When either cannot be read only
// the text can tell, compared the Windows way, since only Windows asks.
func SameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(fa, fb)
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// shellExecuteInfo is SHELLEXECUTEINFOW. It lives in this file, not the Windows one,
// so that a test on any 64-bit host checks its layout.
type shellExecuteInfo struct {
	size     uint32
	mask     uint32
	hwnd     uintptr
	verb     *uint16
	file     *uint16
	params   *uint16
	dir      *uint16
	show     int32
	instApp  uintptr
	idList   uintptr
	class    *uint16
	keyClass uintptr
	hotKey   uint32
	icon     uintptr
	process  uintptr
}
