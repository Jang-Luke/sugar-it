package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sugarintegration/internal/control"
	"sugarintegration/internal/update"
	"sugarintegration/internal/vfs"
)

// updateFake replaces every effect of updating and records them.
type updateFake struct {
	mu          sync.Mutex
	release     update.Release
	newer       bool
	checkErr    error
	checks      atomic.Int32
	downloadErr error
	downloadGo  chan struct{} // if set, a download waits for it
	downloads   atomic.Int32
	helperErr   error
	helperExe   string
	helperArgs  []update.HelperArgs
	opened      []string
	image       string
	imageErr    error
	waitErr     error
	restarts    []string
	restartErr  error
	markerErr   error
	aborted     int
	tempDirs    []string
	sweeps      int
	self        string
}

const fakeSHA = "abab"

func installableRelease(tag string) update.Release {
	v, _ := update.ParseTag(tag)
	return update.Release{
		Tag: tag, Version: v,
		Installer: &update.Asset{URL: update.DownloadURL + tag + "/" + update.InstallerAsset, Size: 7},
		Checksum:  &update.Asset{URL: update.DownloadURL + tag + "/" + update.ChecksumAsset, Size: 7},
	}
}

func updateApp(t *testing.T, goos, ver string) (*App, *updateFake, *windowSpy) {
	t.Helper()
	f := &updateFake{}
	dir := t.TempDir()
	f.self = filepath.Join(dir, "sugar-it-desktop.exe")
	if err := os.WriteFile(f.self, []byte("the program"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, t.TempDir())
	a.tray = &fakeTray{}
	spy := &windowSpy{}
	a.win = windowOps{
		show:       func(context.Context) { spy.shows.Add(1) },
		hide:       func(context.Context) { spy.hides.Add(1) },
		unminimise: func(context.Context) { spy.unmins.Add(1) },
		quit:       func(context.Context) { spy.quits.Add(1) },
	}
	a.goos = goos
	a.version = ver
	a.ctx = context.Background()
	a.upd.interval = time.Hour
	a.upd.deps = updateDeps{
		check: func(context.Context) (update.Release, bool, error) {
			f.checks.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.release, f.newer, f.checkErr
		},
		download: func(ctx context.Context, rel update.Release, dir string, progress func(done, total int64)) (update.Downloaded, error) {
			f.downloads.Add(1)
			if f.downloadGo != nil {
				<-f.downloadGo
			}
			if f.downloadErr != nil {
				return update.Downloaded{}, f.downloadErr
			}
			progress(3, 7)
			p := filepath.Join(dir, update.InstallerAsset)
			return update.Downloaded{Path: p, SHA256: fakeSHA}, os.WriteFile(p, []byte("installer"), 0o700)
		},
		mkTemp: func() (string, error) {
			d, err := os.MkdirTemp(t.TempDir(), update.TempPrefix+"*")
			f.tempDirs = append(f.tempDirs, d)
			return d, err
		},
		selfExe: func() (string, error) { return f.self, nil },
		startHelper: func(exe, dir string, h update.HelperArgs) (func(), error) {
			if f.helperErr != nil {
				return nil, f.helperErr
			}
			f.helperExe = exe
			f.helperArgs = append(f.helperArgs, h)
			return func() { f.aborted++ }, nil
		},
		waitMarker: func(string, time.Duration) error { return f.markerErr },
		procImage:  func(int) (string, error) { return f.image, f.imageErr },
		waitExit:   func(int, time.Duration) error { return f.waitErr },
		restartDaemon: func(app, cfg string) error {
			f.restarts = append(f.restarts, app+"|"+cfg)
			return f.restartErr
		},
		openURL: func(_ context.Context, u string) { f.opened = append(f.opened, u) },
		sweep:   func() { f.sweeps++ },
	}
	t.Cleanup(a.stopUpdates)
	return a, f, spy
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got, msg := parseErr(t, err); got != code {
		t.Errorf("code %q (%s), want %q", got, msg, code)
	}
}

func TestDevBuildDoesNothing(t *testing.T) {
	a, f, _ := updateApp(t, "windows", "dev")
	a.startUpdates()
	time.Sleep(20 * time.Millisecond)
	if f.checks.Load() != 0 {
		t.Errorf("%d checks", f.checks.Load())
	}
	_, err := a.CheckUpdate()
	wantCode(t, err, codeUpdateDev)
	if f.checks.Load() != 0 {
		t.Errorf("CheckUpdate checked: %d", f.checks.Load())
	}
	if info := a.AppState().Update; info.Current != "dev" || info.Available {
		t.Errorf("Update = %+v", info)
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	if version != "dev" {
		t.Errorf("version = %q", version)
	}
}

func TestBackgroundCheckFindsAndRepeats(t *testing.T) {
	a, f, _ := updateApp(t, "windows", "1.2.3")
	f.release, f.newer = installableRelease("v1.2.4"), true
	a.upd.interval = 10 * time.Millisecond
	a.startUpdates()
	deadline := time.Now().Add(5 * time.Second)
	for f.checks.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.checks.Load() < 3 {
		t.Fatalf("%d checks", f.checks.Load())
	}
	info := a.AppState().Update
	if !info.Available || !info.OneClick || info.Latest != "1.2.4" || info.Current != "1.2.3" {
		t.Errorf("Update = %+v", info)
	}
	a.stopUpdates()
	n := f.checks.Load()
	time.Sleep(50 * time.Millisecond)
	if f.checks.Load() > n+1 {
		t.Error("the checks went on after stopUpdates")
	}
}

func TestDefaultIntervalIsSixHours(t *testing.T) {
	if a := newApp(t, t.TempDir()); a.upd.interval != 6*time.Hour {
		t.Errorf("interval %v", a.upd.interval)
	}
}

func TestStartupDoesNotCheckOnADevBuildByDefault(t *testing.T) {
	a := newApp(t, t.TempDir())
	if a.version != "dev" {
		t.Errorf("a.version = %q", a.version)
	}
}

func TestBackgroundFailureIsSilentAndKeepsTheLastResult(t *testing.T) {
	a, f, _ := updateApp(t, "windows", "1.2.3")
	f.release, f.newer = installableRelease("v1.2.4"), true
	a.backgroundCheck(context.Background())
	f.mu.Lock()
	f.checkErr, f.newer = errors.New("GitHub answered 403"), false
	f.mu.Unlock()
	a.backgroundCheck(context.Background())
	if f.checks.Load() != 2 {
		t.Errorf("%d checks, want no retry", f.checks.Load())
	}
	s := a.AppState()
	if !s.Update.Available || s.Update.Latest != "1.2.4" {
		t.Errorf("Update = %+v", s.Update)
	}
	if s.StartError != "" || s.Failure != "" {
		t.Errorf("AppState = %+v", s)
	}
}

func TestCheckUpdateReportsNotNewerAndErrors(t *testing.T) {
	a, f, _ := updateApp(t, "linux", "1.2.3")
	f.release = installableRelease("v1.2.3")
	info, err := a.CheckUpdate()
	if err != nil || info.Available {
		t.Errorf("%+v %v", info, err)
	}
	f.checkErr = errors.New("GitHub answered 403")
	_, err = a.CheckUpdate()
	wantCode(t, err, codeUpdateCheck)
	f.checkErr = update.ErrNoRelease
	_, err = a.CheckUpdate()
	wantCode(t, err, codeUpdateNoRelease)
}

func TestOneClickOnlyOnWindowsWithBothAssets(t *testing.T) {
	for _, c := range []struct {
		goos string
		rel  update.Release
		want bool
	}{
		{"windows", installableRelease("v1.2.4"), true},
		{"linux", installableRelease("v1.2.4"), false},
		{"darwin", installableRelease("v1.2.4"), false},
		{"windows", update.Release{Tag: "v1.2.4", Version: update.Version{Major: 1, Minor: 2, Patch: 4}}, false},
	} {
		a, f, _ := updateApp(t, c.goos, "1.2.3")
		f.release, f.newer = c.rel, true
		info, err := a.CheckUpdate()
		if err != nil || !info.Available || info.OneClick != c.want {
			t.Errorf("%s %+v: %+v %v", c.goos, c.rel, info, err)
		}
	}
}

func TestOpenReleasePage(t *testing.T) {
	a, f, _ := updateApp(t, "linux", "1.2.3")
	if err := a.OpenReleasePage(); err != nil {
		t.Fatal(err)
	}
	f.release, f.newer = installableRelease("v1.2.4"), true
	if _, err := a.CheckUpdate(); err != nil {
		t.Fatal(err)
	}
	if err := a.OpenReleasePage(); err != nil {
		t.Fatal(err)
	}
	want := []string{update.ReleasesURL + "latest", update.ReleasesURL + "tag/v1.2.4"}
	if strings.Join(f.opened, " ") != strings.Join(want, " ") {
		t.Errorf("opened %v", f.opened)
	}
	for _, u := range f.opened {
		if !update.IsReleaseURL(u) {
			t.Errorf("%s is not a release URL", u)
		}
	}
}

func TestOpenReleasePageRefusesAnythingElse(t *testing.T) {
	a, f, _ := updateApp(t, "linux", "1.2.3")
	f.release, f.newer = update.Release{Tag: "../../evil", Version: update.Version{Major: 9}}, true
	if _, err := a.CheckUpdate(); err != nil {
		t.Fatal(err)
	}
	wantCode(t, a.OpenReleasePage(), codeUpdateBadURL)
	if len(f.opened) != 0 {
		t.Errorf("opened %v", f.opened)
	}
}

func TestApplyUpdateOnlyOnWindows(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		a, f, _ := updateApp(t, goos, "1.2.3")
		f.release, f.newer = installableRelease("v1.2.4"), true
		if _, err := a.CheckUpdate(); err != nil {
			t.Fatal(err)
		}
		wantCode(t, a.ApplyUpdate(), codeUpdateUnsupported)
		if f.downloads.Load() != 0 {
			t.Errorf("%s: downloaded", goos)
		}
	}
}

func readyToApply(t *testing.T) (*App, *updateFake, *windowSpy) {
	t.Helper()
	a, f, spy := updateApp(t, "windows", "1.2.3")
	f.release, f.newer = installableRelease("v1.2.4"), true
	if _, err := a.CheckUpdate(); err != nil {
		t.Fatal(err)
	}
	return a, f, spy
}

func TestApplyUpdateStartsTheHelperAndQuits(t *testing.T) {
	a, f, spy := readyToApply(t)
	a.configDir = t.TempDir()
	if err := a.ApplyUpdate(); err != nil {
		t.Fatal(err)
	}
	if len(f.helperArgs) != 1 {
		t.Fatalf("helper started %d times", len(f.helperArgs))
	}
	h := f.helperArgs[0]
	dir := f.tempDirs[0]
	if h.PID != os.Getpid() || h.App != f.self || h.ConfigDir != a.configDir || h.RestartDaemon {
		t.Errorf("helper args %+v", h)
	}
	if h.Installer != filepath.Join(dir, update.InstallerAsset) || h.SHA256 != fakeSHA {
		t.Errorf("installer %s, hash %s", h.Installer, h.SHA256)
	}
	if f.helperExe != filepath.Join(dir, update.HelperExe) {
		t.Errorf("helper %s", f.helperExe)
	}
	b, err := os.ReadFile(f.helperExe)
	if err != nil || string(b) != "the program" {
		t.Errorf("the helper is not a copy of the program: %q %v", b, err)
	}
	if !a.quitting.Load() || spy.quits.Load() != 1 {
		t.Errorf("quitting %v, quits %d", a.quitting.Load(), spy.quits.Load())
	}
	if closeAction(true, a.quitting.Load()) {
		t.Error("closing would hide the window")
	}
}

func TestApplyUpdateWithoutARelease(t *testing.T) {
	a, f, _ := updateApp(t, "windows", "1.2.3")
	wantCode(t, a.ApplyUpdate(), codeUpdateNone)
	if f.downloads.Load() != 0 {
		t.Error("downloaded")
	}
}

func TestApplyUpdateRefusesAReleaseWithoutTheAssets(t *testing.T) {
	a, f, spy := updateApp(t, "windows", "1.2.3")
	f.release, f.newer = update.Release{Tag: "v1.2.4", Version: update.Version{Major: 1, Minor: 2, Patch: 4}}, true
	if _, err := a.CheckUpdate(); err != nil {
		t.Fatal(err)
	}
	wantCode(t, a.ApplyUpdate(), codeUpdateNoInstaller)
	if f.downloads.Load() != 0 || spy.quits.Load() != 0 {
		t.Errorf("downloads %d, quits %d", f.downloads.Load(), spy.quits.Load())
	}
}

func TestApplyUpdateSecondClickIsIgnored(t *testing.T) {
	a, f, spy := readyToApply(t)
	f.downloadGo = make(chan struct{})
	first := make(chan error, 1)
	go func() { first <- a.ApplyUpdate() }()
	deadline := time.Now().Add(5 * time.Second)
	for f.downloads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := a.ApplyUpdate(); err != nil {
		t.Errorf("second click: %v", err)
	}
	if f.downloads.Load() != 1 {
		t.Errorf("%d downloads", f.downloads.Load())
	}
	if p := a.AppState().Update.Phase; p != phaseDownloading {
		t.Errorf("phase %q", p)
	}
	close(f.downloadGo)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if spy.quits.Load() != 1 {
		t.Errorf("quits %d", spy.quits.Load())
	}
}

// Failure at any stage leaves the app running and able to try again, and no files.
func TestApplyUpdateFailuresLeaveTheAppRunning(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*updateFake)
		code string
	}{
		{"download", func(f *updateFake) { f.downloadErr = errors.New("reset") }, codeUpdateDownload},
		{"checksum", func(f *updateFake) { f.downloadErr = update.ErrChecksumMismatch }, codeUpdateChecksum},
		{"no checksum", func(f *updateFake) { f.downloadErr = update.ErrNoChecksum }, codeUpdateNoChecksum},
		{"insecure", func(f *updateFake) { f.downloadErr = update.ErrInsecure }, codeUpdateDownload},
		{"too large", func(f *updateFake) { f.downloadErr = update.ErrTooLarge }, codeUpdateTooLarge},
		{"helper", func(f *updateFake) { f.helperErr = errors.New("blocked") }, codeUpdateHelper},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, f, spy := readyToApply(t)
			c.set(f)
			wantCode(t, a.ApplyUpdate(), c.code)
			if a.quitting.Load() || spy.quits.Load() != 0 {
				t.Error("the app is quitting")
			}
			if p := a.AppState().Update.Phase; p != "" {
				t.Errorf("phase %q", p)
			}
			if _, err := os.Stat(f.tempDirs[0]); !os.IsNotExist(err) {
				t.Errorf("temp directory stayed: %v", err)
			}
			f.downloadErr, f.helperErr = nil, nil
			if err := a.ApplyUpdate(); err != nil {
				t.Errorf("second attempt: %v", err)
			}
		})
	}
}

// daemonBackend counts Shutdown calls of an external daemon.
type daemonBackend struct {
	Backend
	shutdowns atomic.Int32
	err       error
}

func (d *daemonBackend) Shutdown(context.Context) error {
	d.shutdowns.Add(1)
	return d.err
}

// attached makes a an app attached to an external daemon with some other PID.
func attached(t *testing.T, a *App) *daemonBackend {
	t.Helper()
	a.configDir = t.TempDir()
	if err := control.WriteRuntime(a.configDir, control.Runtime{ControlAddr: "127.0.0.1:1", PID: 4242}); err != nil {
		t.Fatal(err)
	}
	be := &daemonBackend{}
	a.be = be
	return be
}

func TestApplyUpdateStopsADaemonOfThisProgram(t *testing.T) {
	a, f, _ := readyToApply(t)
	be := attached(t, a)
	f.image = f.self
	if err := a.ApplyUpdate(); err != nil {
		t.Fatal(err)
	}
	if be.shutdowns.Load() != 1 {
		t.Errorf("%d shutdowns", be.shutdowns.Load())
	}
	if !f.helperArgs[0].RestartDaemon {
		t.Error("the helper will not restart the daemon")
	}
}

func TestApplyUpdateLeavesAnotherProgramsDaemon(t *testing.T) {
	a, f, _ := readyToApply(t)
	be := attached(t, a)
	f.image = filepath.Join(filepath.Dir(f.self), "sugar-it.exe")
	if err := os.WriteFile(f.image, []byte("the CLI"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyUpdate(); err != nil {
		t.Fatal(err)
	}
	if be.shutdowns.Load() != 0 || f.helperArgs[0].RestartDaemon {
		t.Errorf("shutdowns %d, args %+v", be.shutdowns.Load(), f.helperArgs[0])
	}
}

func TestApplyUpdateIgnoresADaemonItCannotIdentify(t *testing.T) {
	a, f, _ := readyToApply(t)
	be := attached(t, a)
	f.imageErr = errors.New("no such process")
	if err := a.ApplyUpdate(); err != nil {
		t.Fatal(err)
	}
	if be.shutdowns.Load() != 0 {
		t.Errorf("%d shutdowns", be.shutdowns.Load())
	}
}

func TestApplyUpdateDaemonRefusesToStop(t *testing.T) {
	a, f, spy := readyToApply(t)
	be := attached(t, a)
	be.err = vfs.E(vfs.CodeInternal, "no")
	f.image = f.self
	wantCode(t, a.ApplyUpdate(), codeUpdateDaemon)
	if len(f.helperArgs) != 0 || spy.quits.Load() != 0 || len(f.restarts) != 0 {
		t.Errorf("helper %v, quits %d, restarts %v", f.helperArgs, spy.quits.Load(), f.restarts)
	}
}

func TestApplyUpdateDaemonDoesNotExitInTime(t *testing.T) {
	a, f, spy := readyToApply(t)
	attached(t, a)
	f.image, f.waitErr = f.self, update.ErrWaitTimeout
	wantCode(t, a.ApplyUpdate(), codeUpdateDaemon)
	if len(f.helperArgs) != 0 || spy.quits.Load() != 0 {
		t.Errorf("helper %v, quits %d", f.helperArgs, spy.quits.Load())
	}
	if len(f.restarts) != 1 {
		t.Errorf("restarts %v: it accepted the shutdown, so sharing may be off", f.restarts)
	}
}

func TestApplyUpdateSaysSharingIsOffWhenTheDaemonCannotRestart(t *testing.T) {
	a, f, _ := readyToApply(t)
	attached(t, a)
	f.image, f.waitErr, f.restartErr = f.self, update.ErrWaitTimeout, errors.New("refused")
	wantCode(t, a.ApplyUpdate(), codeUpdateDaemonOff)

	a, f, _ = readyToApply(t)
	attached(t, a)
	f.image, f.helperErr, f.restartErr = f.self, errors.New("blocked"), errors.New("refused")
	wantCode(t, a.ApplyUpdate(), codeUpdateDaemonOff)
}

// Defender may remove the helper as it starts: without this, the app would quit and
// nothing would start it again.
func TestApplyUpdateWithAHelperThatNeverStarts(t *testing.T) {
	a, f, spy := readyToApply(t)
	attached(t, a)
	f.image, f.markerErr = f.self, update.ErrHelperGone
	wantCode(t, a.ApplyUpdate(), codeUpdateHelper)
	if a.quitting.Load() || spy.quits.Load() != 0 {
		t.Error("the app is quitting")
	}
	if f.aborted != 1 {
		t.Errorf("aborted %d times", f.aborted)
	}
	if len(f.restarts) != 1 {
		t.Errorf("restarts %v", f.restarts)
	}
	if p := a.AppState().Update.Phase; p != "" {
		t.Errorf("phase %q", p)
	}
	if _, err := os.Stat(f.tempDirs[0]); !os.IsNotExist(err) {
		t.Errorf("temp directory stayed: %v", err)
	}
}

// The helper gave up waiting for this app, which is still running: it must say so
// and be able to try again.
func TestRunningAppPicksUpTheHelperStatus(t *testing.T) {
	a, f, spy := readyToApply(t)
	a.configDir = t.TempDir()
	if err := a.ApplyUpdate(); err != nil {
		t.Fatal(err)
	}
	if !a.quitting.Load() || spy.quits.Load() != 1 {
		t.Fatal("not quitting")
	}
	if n := a.AppState().Update.Notice; n != "" {
		t.Errorf("notice %q before any status", n)
	}
	if err := update.WriteStatus(a.configDir, update.Status{Outcome: update.OutcomeTimeout, Detail: "d", DaemonOff: true}); err != nil {
		t.Fatal(err)
	}
	info := a.AppState().Update
	if info.Notice != update.OutcomeTimeout || info.NoticeDetail != "d" || !info.NoticeDaemonOff {
		t.Errorf("Update = %+v", info)
	}
	if a.quitting.Load() {
		t.Error("still quitting, so closing would not hide the window")
	}
	if err := a.ApplyUpdate(); err != nil {
		t.Errorf("a new attempt: %v", err)
	}
	if len(f.helperArgs) != 2 {
		t.Errorf("helper started %d times", len(f.helperArgs))
	}
}

// Reopened during the UAC prompt, this app holds the lock, so the helper's relaunch
// exits and nothing else would ever show why the update did nothing.
func TestAReopenedAppShowsTheHelperStatus(t *testing.T) {
	a, _, _ := readyToApply(t)
	a.configDir = t.TempDir()
	if err := update.WriteStatus(a.configDir, update.Status{Outcome: update.OutcomeInstallerFailed}); err != nil {
		t.Fatal(err)
	}
	if n := a.AppState().Update.Notice; n != update.OutcomeInstallerFailed {
		t.Errorf("notice %q", n)
	}
	if a.upd.busy.Load() {
		t.Error("busy")
	}
}

func TestApplyUpdateRestartsTheDaemonIfTheHelperCannotStart(t *testing.T) {
	a, f, spy := readyToApply(t)
	attached(t, a)
	f.image, f.helperErr = f.self, errors.New("blocked")
	wantCode(t, a.ApplyUpdate(), codeUpdateHelper)
	if len(f.restarts) != 1 || f.restarts[0] != f.self+"|"+a.configDir {
		t.Errorf("restarts %v", f.restarts)
	}
	if spy.quits.Load() != 0 {
		t.Error("quit")
	}
}

func TestApplyUpdateWithAnInProcessDaemon(t *testing.T) {
	dir := t.TempDir()
	a, f, _ := readyToApply(t)
	a.configDir = dir
	a.startup(context.Background())
	if a.d == nil {
		t.Fatalf("no in-process daemon: %+v", a.AppState())
	}
	defer a.shutdown(context.Background())
	f.image = f.self // the runtime file's PID is this process, which must not stop itself
	if err := a.ApplyUpdate(); err != nil {
		t.Fatal(err)
	}
	if f.helperArgs[0].RestartDaemon {
		t.Error("restart requested for an in-process daemon")
	}
}

func TestStatusFromAFailedUpdateIsShownOnce(t *testing.T) {
	a, f, _ := updateApp(t, "windows", "1.2.3")
	a.configDir = t.TempDir()
	if err := update.WriteStatus(a.configDir, update.Status{Outcome: update.OutcomeDenied, Detail: "d"}); err != nil {
		t.Fatal(err)
	}
	a.startUpdates()
	info := a.AppState().Update
	if info.Notice != update.OutcomeDenied || info.NoticeDetail != "d" {
		t.Errorf("Update = %+v", info)
	}
	if f.sweeps != 1 {
		t.Errorf("%d sweeps", f.sweeps)
	}
	a.DismissUpdateNotice()
	if a.AppState().Update.Notice != "" {
		t.Error("the notice stayed")
	}
	if _, err := os.Stat(filepath.Join(a.configDir, update.StatusFile)); !os.IsNotExist(err) {
		t.Errorf("status file stayed: %v", err)
	}
}

func TestOnlyWindowsReadsStatusOrSweeps(t *testing.T) {
	a, f, _ := updateApp(t, "linux", "1.2.3")
	a.configDir = t.TempDir()
	if err := update.WriteStatus(a.configDir, update.Status{Outcome: update.OutcomeDenied}); err != nil {
		t.Fatal(err)
	}
	a.startUpdates()
	if a.AppState().Update.Notice != "" || f.sweeps != 0 {
		t.Errorf("Update = %+v, sweeps %d", a.AppState().Update, f.sweeps)
	}
}

func TestRunUpdateHelperIsHandledBeforeTheGUI(t *testing.T) {
	var out strings.Builder
	code, handled := runUpdateHelper([]string{update.HelperCommand}, &out)
	if !handled || code != 2 || !strings.Contains(out.String(), "usage") {
		t.Errorf("code %d, handled %v, output %q", code, handled, out.String())
	}
	if _, handled := runUpdateHelper(nil, &out); handled {
		t.Error("no arguments handled")
	}
	if _, handled := runUpdateHelper([]string{"daemon"}, &out); handled {
		t.Error("daemon handled")
	}
}

// The front end shows every code the Go side can return; a new one needs its text.
func TestKoHasEveryUpdateCode(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("frontend", "src", "ko.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{
		codeUpdateDev, codeUpdateUnsupported, codeUpdateNone, codeUpdateCheck, codeUpdateNoRelease,
		codeUpdateNoInstaller, codeUpdateDownload, codeUpdateTooLarge, codeUpdateNoChecksum,
		codeUpdateChecksum, codeUpdateHelper, codeUpdateDaemon, codeUpdateDaemonOff, codeUpdateBadURL,
		update.OutcomeDenied, update.OutcomeInstallerFailed, update.OutcomeTimeout,
		update.OutcomeInstallerChanged, update.OutcomeInstallerTimeout, update.OutcomeDaemonRestart,
	} {
		if !strings.Contains(string(b), code+":") {
			t.Errorf("ko.ts has no text for %s", code)
		}
	}
}
