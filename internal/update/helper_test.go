package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

var testSHA = strings.Repeat("ab", 32)

func TestHelperArgsRoundTrip(t *testing.T) {
	a := HelperArgs{PID: 42, Installer: `C:\Temp dir\sugar-it-update-1\a.exe`, SHA256: testSHA, App: `C:\Program Files\x\app.exe`, ConfigDir: `C:\c d`, RestartDaemon: true}
	got, err := ParseHelperArgs(a.Argv()[1:])
	if err != nil {
		t.Fatal(err)
	}
	if got != a {
		t.Errorf("got %+v", got)
	}
	if a.Argv()[0] != HelperCommand {
		t.Errorf("Argv()[0] = %q", a.Argv()[0])
	}
	plain := HelperArgs{PID: 1, Installer: "i", SHA256: testSHA, App: "a"}
	want := []string{"update-helper", "--pid", "1", "--installer", "i", "--sha256", testSHA, "--app", "a"}
	if !reflect.DeepEqual(plain.Argv(), want) {
		t.Errorf("Argv = %v", plain.Argv())
	}
}

func TestParseHelperArgsRefusesBadInput(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--pid", "1", "--installer", "i", "--sha256", testSHA},
		{"--pid", "1", "--sha256", testSHA, "--app", "a"},
		{"--installer", "i", "--sha256", testSHA, "--app", "a"},
		{"--pid", "0", "--installer", "i", "--sha256", testSHA, "--app", "a"},
		{"--pid", "-1", "--installer", "i", "--sha256", testSHA, "--app", "a"},
		{"--pid", "1", "--installer", "i", "--sha256", testSHA, "--app", "a", "extra"},
		{"--pid", "1", "--installer", "i", "--app", "a"},
		{"--pid", "1", "--installer", "i", "--sha256", "abcd", "--app", "a"},
		{"--pid", "1", "--installer", "i", "--sha256", strings.Repeat("zz", 32), "--app", "a"},
		{"--nope"},
	} {
		if _, err := ParseHelperArgs(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestInstallerParams(t *testing.T) {
	// NSIS wants /D= last and unquoted, spaces included.
	if got, want := InstallerParams(`C:\Program Files\sugar-it-desktop`), `/S /D=C:\Program Files\sugar-it-desktop`; got != want {
		t.Errorf("got %q", got)
	}
}

func TestRelaunchArgs(t *testing.T) {
	if got := AppArgv(""); len(got) != 0 {
		t.Errorf("AppArgv = %v", got)
	}
	if got := AppArgv("c d"); !reflect.DeepEqual(got, []string{"--config-dir", "c d"}) {
		t.Errorf("AppArgv = %v", got)
	}
	if got := DaemonArgv(""); !reflect.DeepEqual(got, []string{"daemon"}) {
		t.Errorf("DaemonArgv = %v", got)
	}
	if got := DaemonArgv("c d"); !reflect.DeepEqual(got, []string{"daemon", "--config-dir", "c d"}) {
		t.Errorf("DaemonArgv = %v", got)
	}
}

// recorder is a fake platform that lists what the helper did, in order.
type recorder struct {
	calls     []string
	waitErr   error
	exit      int
	runErrs   []error // one per RunElevated call; the last repeats
	holdErr   error
	held      bool
	appHash   string // "" means the install changed the program
	hashes    map[string]string
	daemonErr error
	status    *Status
}

const oldProgram, newProgram = "old-program-hash", "new-program-hash"

func (r *recorder) hash(path string) (string, error) {
	switch path {
	case helperSelf:
		return oldProgram, nil
	case helperArgs.App:
		if r.appHash != "" {
			return r.appHash, nil
		}
		return newProgram, nil
	case helperArgs.Installer:
		if h, ok := r.hashes[path]; ok {
			return h, nil
		}
		return testSHA, nil
	}
	return "", fmt.Errorf("no such file %s", path)
}

type holdCloser struct{ r *recorder }

func (h holdCloser) Close() error { h.r.held = false; return nil }

func (r *recorder) ops() HelperOps {
	return HelperOps{
		SelfExe:  func() (string, error) { return helperSelf, nil },
		HashFile: r.hash,
		Mark: func(dir string) error {
			r.calls = append(r.calls, "mark:"+filepath.Base(dir))
			return nil
		},
		WaitExit: func(pid int, d time.Duration) error {
			r.calls = append(r.calls, "wait")
			if d != HelperWait {
				r.calls = append(r.calls, "bad wait")
			}
			return r.waitErr
		},
		Hold: func(file, want string) (io.Closer, error) {
			r.calls = append(r.calls, "hold")
			if want != testSHA {
				r.calls = append(r.calls, "bad hash")
			}
			if r.holdErr != nil {
				return nil, r.holdErr
			}
			r.held = true
			return holdCloser{r}, nil
		},
		RunElevated: func(file, params string) (int, error) {
			c := "install:" + filepath.Base(file) + ":" + params
			if r.held {
				c += ":held"
			}
			r.calls = append(r.calls, c)
			var err error
			if len(r.runErrs) > 0 {
				err = r.runErrs[0]
				if len(r.runErrs) > 1 {
					r.runErrs = r.runErrs[1:]
				}
			}
			return r.exit, err
		},
		StartDaemon: func(app, cfg string) error {
			r.calls = append(r.calls, "daemon:"+cfg)
			return r.daemonErr
		},
		StartApp: func(app, cfg string) error {
			r.calls = append(r.calls, "app:"+cfg)
			return nil
		},
		WriteStatus: func(s Status) error {
			r.calls = append(r.calls, "status:"+s.Outcome)
			r.status = &s
			return nil
		},
		RemoveAll: func(dir string) {
			if r.held {
				r.calls = append(r.calls, "rm while held")
			}
			r.calls = append(r.calls, "rm:"+filepath.Base(dir))
		},
	}
}

const helperSelf = "/tmp/sugar-it-update-1/" + HelperExe

var helperArgs = HelperArgs{PID: 9, Installer: "/tmp/sugar-it-update-1/setup.exe", SHA256: testSHA, App: "/opt/app/sugar-it-desktop.exe", ConfigDir: "/c"}

// installCall is how the recorder lists the installer, in the OS's path form.
var installCall = "install:setup.exe:/S /D=" + filepath.Dir(helperArgs.App) + ":held"

const markCall = "mark:sugar-it-update-1"

func TestHelperSuccess(t *testing.T) {
	r := &recorder{}
	if code := RunHelper(helperArgs, r.ops()); code != 0 {
		t.Errorf("code %d", code)
	}
	want := []string{markCall, "wait", "hold", installCall, "app:/c", "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperRestartsTheDaemonBeforeTheApp(t *testing.T) {
	// The other way round the app would host a daemon and the service would be refused.
	r := &recorder{}
	a := helperArgs
	a.RestartDaemon = true
	RunHelper(a, r.ops())
	want := []string{markCall, "wait", "hold", installCall, "daemon:/c", "app:/c", "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperUACRefusal(t *testing.T) {
	r := &recorder{runErrs: []error{ErrElevationDenied}, appHash: oldProgram}
	a := helperArgs
	a.RestartDaemon = true
	if code := RunHelper(a, r.ops()); code == 0 {
		t.Error("code 0")
	}
	want := []string{markCall, "wait", "hold", installCall, "daemon:/c", "status:" + OutcomeDenied, "app:/c", "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperInstallerFailures(t *testing.T) {
	r := &recorder{exit: 2}
	RunHelper(helperArgs, r.ops())
	if r.status == nil || r.status.Outcome != OutcomeInstallerFailed || r.status.Code != 2 {
		t.Errorf("status %+v", r.status)
	}
	r = &recorder{runErrs: []error{errors.New("boom")}}
	RunHelper(helperArgs, r.ops())
	if r.status == nil || r.status.Outcome != OutcomeInstallerFailed || r.status.Detail != "boom" {
		t.Errorf("status %+v", r.status)
	}
	if r.calls[len(r.calls)-2] != "app:/c" {
		t.Errorf("no relaunch: %v", r.calls)
	}
}

// Silent NSIS skips a locked file and still exits 0, so the exit code proves nothing.
func TestHelperExitZeroWithTheProgramUnchangedIsAFailure(t *testing.T) {
	r := &recorder{appHash: oldProgram}
	if code := RunHelper(helperArgs, r.ops()); code == 0 {
		t.Error("code 0")
	}
	if r.status == nil || r.status.Outcome != OutcomeInstallerFailed || !strings.Contains(r.status.Detail, "unchanged") {
		t.Errorf("status %+v", r.status)
	}
	if r.calls[len(r.calls)-2] != "app:/c" {
		t.Errorf("no relaunch: %v", r.calls)
	}
}

func TestHelperCannotReadTheProgramAfterInstalling(t *testing.T) {
	r := &recorder{}
	ops := r.ops()
	ops.HashFile = func(path string) (string, error) {
		if path == helperArgs.App {
			return "", errors.New("gone")
		}
		return r.hash(path)
	}
	RunHelper(helperArgs, ops)
	if r.status == nil || r.status.Outcome != OutcomeInstallerFailed {
		t.Errorf("status %+v", r.status)
	}
}

// The app outlived the wait: it holds the single-instance lock, so a relaunch would
// only exit, and installing would hit a locked file. The running app shows the status.
func TestHelperDoesNothingWhileTheAppRuns(t *testing.T) {
	r := &recorder{waitErr: ErrWaitTimeout}
	if code := RunHelper(helperArgs, r.ops()); code == 0 {
		t.Error("code 0")
	}
	want := []string{markCall, "wait", "status:" + OutcomeTimeout, "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperRestartsTheDaemonWhileTheAppRuns(t *testing.T) {
	r := &recorder{waitErr: ErrWaitTimeout}
	a := helperArgs
	a.RestartDaemon = true
	RunHelper(a, r.ops())
	want := []string{markCall, "wait", "daemon:/c", "status:" + OutcomeTimeout, "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

// Not knowing whether the app runs: not installing is safe, and a relaunch is too,
// since it only shows the window if the app is there.
func TestHelperWaitError(t *testing.T) {
	r := &recorder{waitErr: errors.New("access denied")}
	RunHelper(helperArgs, r.ops())
	want := []string{markCall, "wait", "status:" + OutcomeTimeout, "app:/c", "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperInstallerStillRunning(t *testing.T) {
	timedOut := fmt.Errorf("wait for the installer: %w", ErrWaitTimeout)
	for _, c := range []struct {
		name, appHash, want string
	}{
		{"unchanged", oldProgram, "unchanged"},
		{"changed", newProgram, "replaced"},
	} {
		r := &recorder{runErrs: []error{timedOut}, appHash: c.appHash}
		if code := RunHelper(helperArgs, r.ops()); code == 0 {
			t.Errorf("%s: code 0", c.name)
		}
		if r.status == nil || r.status.Outcome != OutcomeInstallerTimeout || !strings.Contains(r.status.Detail, c.want) {
			t.Errorf("%s: status %+v", c.name, r.status)
		}
		if r.calls[len(r.calls)-2] != "app:/c" {
			t.Errorf("%s: no relaunch: %v", c.name, r.calls)
		}
	}
}

func TestHelperRefusesAnInstallerThatChanged(t *testing.T) {
	r := &recorder{holdErr: ErrInstallerChanged}
	if code := RunHelper(helperArgs, r.ops()); code == 0 {
		t.Error("code 0")
	}
	want := []string{markCall, "wait", "hold", "status:" + OutcomeInstallerChanged, "app:/c", "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

// Should the held handle keep the installer from starting, it is hashed once more
// and started without the handle, which is the remaining gap.
func TestHelperFallsBackWhenTheHandleBlocksExecution(t *testing.T) {
	r := &recorder{runErrs: []error{ErrInstallerBusy, nil}}
	if code := RunHelper(helperArgs, r.ops()); code != 0 {
		t.Errorf("code %d", code)
	}
	unheld := strings.TrimSuffix(installCall, ":held")
	want := []string{markCall, "wait", "hold", installCall, unheld, "app:/c", "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperFallbackStillChecksTheHash(t *testing.T) {
	r := &recorder{runErrs: []error{ErrInstallerBusy}, hashes: map[string]string{helperArgs.Installer: "other"}}
	RunHelper(helperArgs, r.ops())
	if r.status == nil || r.status.Outcome != OutcomeInstallerChanged {
		t.Errorf("status %+v", r.status)
	}
	if n := strings.Count(strings.Join(r.calls, " "), "install:"); n != 1 {
		t.Errorf("%d installs: %v", n, r.calls)
	}
}

func TestHelperReportsAFailedDaemonRestart(t *testing.T) {
	r := &recorder{daemonErr: errors.New("refused")}
	a := helperArgs
	a.RestartDaemon = true
	if code := RunHelper(a, r.ops()); code == 0 {
		t.Error("code 0")
	}
	if r.status == nil || r.status.Outcome != OutcomeDaemonRestart || !r.status.DaemonOff || !strings.Contains(r.status.Detail, "refused") {
		t.Errorf("status %+v", r.status)
	}
	// The app comes after the status, so that it reads it.
	if r.calls[len(r.calls)-3] != "status:"+OutcomeDaemonRestart || r.calls[len(r.calls)-2] != "app:/c" {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperKeepsTheInstallFailureWhenTheDaemonFailsToo(t *testing.T) {
	r := &recorder{runErrs: []error{ErrElevationDenied}, daemonErr: errors.New("refused")}
	a := helperArgs
	a.RestartDaemon = true
	RunHelper(a, r.ops())
	if r.status == nil || r.status.Outcome != OutcomeDenied || !r.status.DaemonOff {
		t.Errorf("status %+v", r.status)
	}
}

func TestHelperOnlyRemovesItsOwnTempDirectory(t *testing.T) {
	r := &recorder{}
	a := helperArgs
	a.Installer = "/home/me/Documents/setup.exe"
	ops := r.ops()
	ops.HashFile = func(path string) (string, error) {
		if path == helperArgs.App {
			return newProgram, nil
		}
		return oldProgram, nil
	}
	RunHelper(a, ops)
	for _, c := range r.calls {
		if strings.HasPrefix(c, "rm:") {
			t.Errorf("removed %s", c)
		}
	}
}

func TestHoldVerified(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "setup.exe")
	if err := os.WriteFile(p, []byte("installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	open := func(path string) (*os.File, error) { return os.Open(path) }
	want, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := HoldVerified(open, p, want)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("the handle was not left open: %v", err)
	}
	if _, err := HoldVerified(open, p, strings.Repeat("0", 64)); !errors.Is(err, ErrInstallerChanged) {
		t.Errorf("other hash: %v", err)
	}
	if _, err := HoldVerified(open, filepath.Join(dir, "gone"), want); !errors.Is(err, ErrInstallerChanged) {
		t.Errorf("missing file: %v", err)
	}
}

func TestSameHashIgnoresCase(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, _ := HashFile(p)
	if _, err := HoldVerified(func(path string) (*os.File, error) { return os.Open(path) }, p, strings.ToUpper(h)); err != nil {
		t.Error(err)
	}
}

func TestMarker(t *testing.T) {
	dir := t.TempDir()
	if err := WaitMarker(dir, 20*time.Millisecond); !errors.Is(err, ErrHelperGone) {
		t.Errorf("no marker: %v", err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		WriteMarker(dir)
	}()
	if err := WaitMarker(dir, 5*time.Second); err != nil {
		t.Errorf("marker: %v", err)
	}
}

func TestStatusNewOutcomesAreKept(t *testing.T) {
	for _, o := range []string{OutcomeInstallerChanged, OutcomeInstallerTimeout, OutcomeDaemonRestart} {
		dir := t.TempDir()
		if err := WriteStatus(dir, Status{Outcome: o, DaemonOff: true}); err != nil {
			t.Fatal(err)
		}
		s, ok := TakeStatus(dir)
		if !ok || s.Outcome != o || !s.DaemonOff {
			t.Errorf("%s: %+v %v", o, s, ok)
		}
	}
}

func TestStatusFile(t *testing.T) {
	dir := t.TempDir()
	if _, ok := TakeStatus(dir); ok {
		t.Error("status without a file")
	}
	if err := WriteStatus(dir, Status{Outcome: OutcomeDenied}); err != nil {
		t.Fatal(err)
	}
	s, ok := TakeStatus(dir)
	if !ok || s.Outcome != OutcomeDenied {
		t.Errorf("%+v %v", s, ok)
	}
	// Shown once.
	if _, ok := TakeStatus(dir); ok {
		t.Error("status read twice")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("left %v", ents)
	}
}

func TestStatusFileGarbageIsDropped(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{"not json", `{"Outcome":"<img src=x>"}`, `{}`} {
		if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, ok := TakeStatus(dir); ok {
			t.Errorf("%q gave %+v", content, s)
		}
		if _, err := os.Stat(filepath.Join(dir, StatusFile)); !os.IsNotExist(err) {
			t.Errorf("%q stayed", content)
		}
	}
}

func TestSweepTempRemovesOnlyOldUpdateDirectories(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, TempPrefix+"old")
	fresh := filepath.Join(root, TempPrefix+"fresh")
	other := filepath.Join(root, "other-old")
	for _, d := range []string{old, fresh, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, TempPrefix+"file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-2 * SweepAge)
	for _, p := range []string{old, other, file} {
		if err := os.Chtimes(p, long, long); err != nil {
			t.Fatal(err)
		}
	}
	SweepTemp(root, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the old directory stayed")
	}
	for _, p := range []string{fresh, other, file} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Link(a, link); err != nil {
		t.Skip(err)
	}
	if !SameFile(a, a) || !SameFile(a, link) {
		t.Error("the same file differs")
	}
	if SameFile(a, b) {
		t.Error("two files match")
	}
	if SameFile(a, "") || SameFile("", "") {
		t.Error("an empty path matches")
	}
	// Neither can be read: only the text can tell, and Windows ignores case.
	if !SameFile(`C:\gone\APP.EXE`, `c:\gone\app.exe`) {
		t.Error("case-insensitive fallback")
	}
}

// The layout of SHELLEXECUTEINFOW on 64-bit Windows; a wrong one fails the call or
// reads the process handle from the wrong place. It is checked here because this
// host cannot make the call.
func TestShellExecuteInfoLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("64-bit only")
	}
	var s shellExecuteInfo
	if got := unsafe.Sizeof(s); got != 112 {
		t.Errorf("size %d", got)
	}
	if got := unsafe.Offsetof(s.show); got != 48 {
		t.Errorf("nShow at %d", got)
	}
	if got := unsafe.Offsetof(s.process); got != 104 {
		t.Errorf("hProcess at %d", got)
	}
}
