package update

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	"unsafe"
)

func TestHelperArgsRoundTrip(t *testing.T) {
	a := HelperArgs{PID: 42, Installer: `C:\Temp dir\sugar-it-update-1\a.exe`, App: `C:\Program Files\x\app.exe`, ConfigDir: `C:\c d`, RestartDaemon: true}
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
	plain := HelperArgs{PID: 1, Installer: "i", App: "a"}
	want := []string{"update-helper", "--pid", "1", "--installer", "i", "--app", "a"}
	if !reflect.DeepEqual(plain.Argv(), want) {
		t.Errorf("Argv = %v", plain.Argv())
	}
}

func TestParseHelperArgsRefusesBadInput(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--pid", "1", "--installer", "i"},
		{"--pid", "1", "--app", "a"},
		{"--installer", "i", "--app", "a"},
		{"--pid", "0", "--installer", "i", "--app", "a"},
		{"--pid", "-1", "--installer", "i", "--app", "a"},
		{"--pid", "1", "--installer", "i", "--app", "a", "extra"},
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
	calls   []string
	waitErr error
	exit    int
	runErr  error
	status  *Status
}

func (r *recorder) ops() HelperOps {
	return HelperOps{
		WaitExit: func(pid int, d time.Duration) error {
			r.calls = append(r.calls, "wait")
			if d != HelperWait {
				r.calls = append(r.calls, "bad wait")
			}
			return r.waitErr
		},
		RunElevated: func(file, params string) (int, error) {
			r.calls = append(r.calls, "install:"+filepath.Base(file)+":"+params)
			return r.exit, r.runErr
		},
		StartDaemon: func(app, cfg string) error {
			r.calls = append(r.calls, "daemon:"+cfg)
			return nil
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
		RemoveAll: func(dir string) { r.calls = append(r.calls, "rm:"+filepath.Base(dir)) },
	}
}

var helperArgs = HelperArgs{PID: 9, Installer: "/tmp/sugar-it-update-1/setup.exe", App: "/opt/app/sugar-it-desktop.exe", ConfigDir: "/c"}

// installCall is how the recorder lists the installer, in the OS's path form.
var installCall = "install:setup.exe:/S /D=" + filepath.Dir(helperArgs.App)

func TestHelperSuccess(t *testing.T) {
	r := &recorder{}
	if code := RunHelper(helperArgs, r.ops()); code != 0 {
		t.Errorf("code %d", code)
	}
	want := []string{"wait", installCall, "app:/c", "rm:sugar-it-update-1"}
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
	want := []string{"wait", installCall, "daemon:/c", "app:/c", "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperUACRefusal(t *testing.T) {
	r := &recorder{runErr: ErrElevationDenied}
	a := helperArgs
	a.RestartDaemon = true
	if code := RunHelper(a, r.ops()); code == 0 {
		t.Error("code 0")
	}
	want := []string{"wait", installCall, "status:" + OutcomeDenied, "daemon:/c", "app:/c", "rm:sugar-it-update-1"}
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
	r = &recorder{runErr: errors.New("boom")}
	RunHelper(helperArgs, r.ops())
	if r.status == nil || r.status.Outcome != OutcomeInstallerFailed || r.status.Detail != "boom" {
		t.Errorf("status %+v", r.status)
	}
	if r.calls[len(r.calls)-2] != "app:/c" {
		t.Errorf("no relaunch: %v", r.calls)
	}
}

func TestHelperDoesNotInstallWhileTheAppRuns(t *testing.T) {
	r := &recorder{waitErr: ErrWaitTimeout}
	if code := RunHelper(helperArgs, r.ops()); code == 0 {
		t.Error("code 0")
	}
	want := []string{"wait", "status:" + OutcomeTimeout, "app:/c", "rm:sugar-it-update-1"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
}

func TestHelperOnlyRemovesItsOwnTempDirectory(t *testing.T) {
	r := &recorder{}
	a := helperArgs
	a.Installer = "/home/me/Documents/setup.exe"
	RunHelper(a, r.ops())
	for _, c := range r.calls {
		if len(c) > 3 && c[:3] == "rm:" {
			t.Errorf("removed %s", c)
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
