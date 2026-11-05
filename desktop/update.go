package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"

	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
	"sugarintegration/internal/update"
	"sugarintegration/internal/vfs"
)

// Error codes of the update bindings; ko.ts has the text of each.
const (
	codeUpdateDev         = "update_dev"
	codeUpdateUnsupported = "update_unsupported"
	codeUpdateNone        = "update_none"
	codeUpdateCheck       = "update_check_failed"
	codeUpdateNoRelease   = "update_no_release"
	codeUpdateNoInstaller = "update_no_installer"
	codeUpdateDownload    = "update_download_failed"
	codeUpdateTooLarge    = "update_too_large"
	codeUpdateNoChecksum  = "update_no_checksum"
	codeUpdateChecksum    = "update_checksum_mismatch"
	codeUpdateHelper      = "update_helper_failed"
	codeUpdateDaemon      = "update_daemon_stop_failed"
	codeUpdateDaemonOff   = "update_daemon_off"
	codeUpdateBadURL      = "update_bad_url"
)

// The stages of ApplyUpdate, in UpdateInfo.Phase; empty means none is running.
const (
	phaseDownloading = "downloading"
	phasePreparing   = "preparing"
)

// UpdateInfo is the update state the window shows. Only versions are in it: release
// text is untrusted and never reaches the front end.
type UpdateInfo struct {
	Current      string // this build's version
	Latest       string // the latest release's version; "" until a check succeeds
	Available    bool   // Latest is strictly newer than Current
	OneClick     bool   // ApplyUpdate can install it: Windows, with both assets
	Phase        string // the running stage of ApplyUpdate
	Done, Total  int64  // bytes of the download
	Notice       string // why the previous update attempt failed (update.Outcome*)
	NoticeDetail string
	// NoticeDaemonOff: the daemon stopped for that attempt did not start again.
	NoticeDaemonOff bool
}

// updateDeps are the update's effects, seams for tests.
type updateDeps struct {
	check         func(ctx context.Context) (update.Release, bool, error)
	download      func(ctx context.Context, rel update.Release, dir string, progress func(done, total int64)) (update.Downloaded, error)
	mkTemp        func() (string, error)
	selfExe       func() (string, error)
	startHelper   func(helperExe, dir string, a update.HelperArgs) (abort func(), err error)
	waitMarker    func(dir string, d time.Duration) error
	procImage     func(pid int) (string, error)
	waitExit      func(pid int, d time.Duration) error
	restartDaemon func(app, configDir string) error
	openURL       func(ctx context.Context, url string)
	sweep         func()
}

// updater is the App's update state.
type updater struct {
	deps     updateDeps
	interval time.Duration
	busy     atomic.Bool // ApplyUpdate is running; a second click does nothing
	// helperOut: this app started the helper and is quitting; if it is still here when
	// a status arrives, the helper gave up waiting for it.
	helperOut atomic.Bool

	mu        sync.Mutex
	release   *update.Release // the last release a check returned
	available bool
	phase     string
	done      int64
	total     int64
	notice    update.Status
	stop      context.CancelFunc
	stopped   chan struct{}
}

func defaultUpdateDeps(current string) updateDeps {
	checker := update.Checker{BaseURL: update.APIBase, HTTP: &http.Client{}, Current: current}
	downloader := update.Downloader{HTTP: &http.Client{}}
	return updateDeps{
		check:         checker.Check,
		download:      downloader.Installer,
		mkTemp:        func() (string, error) { return os.MkdirTemp("", update.TempPrefix+"*") },
		selfExe:       os.Executable,
		startHelper:   update.StartHelper,
		waitMarker:    update.WaitMarker,
		procImage:     update.ProcessImage,
		waitExit:      update.WaitExit,
		restartDaemon: update.StartDaemon,
		openURL:       wailsrt.BrowserOpenURL,
		sweep:         func() { update.SweepTemp(os.TempDir(), time.Now()) },
	}
}

func updateErr(code string, err error) error {
	return apiError{vfs.Error{Code: code, Message: err.Error()}}
}

func (a *App) updatesEnabled() bool {
	_, ok := update.ParseVersion(a.version)
	return ok
}

// resolvedConfigDir is the absolute config directory.
func (a *App) resolvedConfigDir() (string, error) {
	if a.configDir == "" {
		return config.DefaultDir()
	}
	return filepath.Abs(a.configDir)
}

// startUpdates picks up the result of an update that failed, and starts the check
// at startup and every six hours. A development build checks nothing.
func (a *App) startUpdates() {
	if a.goos == "windows" {
		a.upd.deps.sweep()
		if dir, err := a.resolvedConfigDir(); err == nil {
			if s, ok := update.TakeStatus(dir); ok {
				a.upd.mu.Lock()
				a.upd.notice = s
				a.upd.mu.Unlock()
			}
		}
	}
	if !a.updatesEnabled() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	a.upd.mu.Lock()
	a.upd.stop, a.upd.stopped = cancel, stopped
	interval := a.upd.interval
	a.upd.mu.Unlock()
	go func() {
		defer close(stopped)
		a.backgroundCheck(ctx)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				a.backgroundCheck(ctx)
			}
		}
	}()
}

// stopUpdates ends the periodic check and waits for it. It may run twice.
func (a *App) stopUpdates() {
	a.upd.mu.Lock()
	stop, stopped := a.upd.stop, a.upd.stopped
	a.upd.stop, a.upd.stopped = nil, nil
	a.upd.mu.Unlock()
	if stop != nil {
		stop()
		<-stopped
	}
}

// backgroundCheck is the scheduled check: a failure is logged and shows nothing, and
// the next try is the next tick. It keeps what the last successful check found.
func (a *App) backgroundCheck(ctx context.Context) {
	if err := a.runCheck(ctx); err != nil && ctx.Err() == nil {
		log.Printf("sugar-it: update check: %v", err)
	}
}

func (a *App) runCheck(ctx context.Context) error {
	rel, newer, err := a.upd.deps.check(ctx)
	if err != nil {
		return err
	}
	a.upd.mu.Lock()
	a.upd.release, a.upd.available = &rel, newer
	a.upd.mu.Unlock()
	return nil
}

func (a *App) updateInfo() UpdateInfo {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	info := UpdateInfo{
		Current:      a.version,
		Available:    a.upd.available,
		Phase:        a.upd.phase,
		Done:         a.upd.done,
		Total:        a.upd.total,
		Notice:       a.upd.notice.Outcome,
		NoticeDetail: a.upd.notice.Detail,

		NoticeDaemonOff: a.upd.notice.DaemonOff,
	}
	if r := a.upd.release; r != nil {
		info.Latest = r.Version.String()
		info.OneClick = a.upd.available && a.goos == "windows" && r.Installable()
	}
	return info
}

// pollHelperStatus shows a helper's status in a running app. Startup reads it only
// once, and an app can be running when the helper finishes: one that outlived the
// helper's wait, which is usable again after this, or one the user reopened during
// the UAC prompt, which took the single-instance lock from the helper's relaunch.
func (a *App) pollHelperStatus() {
	if a.goos != "windows" {
		return
	}
	dir, err := a.resolvedConfigDir()
	if err != nil {
		return
	}
	s, ok := update.TakeStatus(dir)
	if !ok {
		return
	}
	a.upd.mu.Lock()
	a.upd.notice = s
	a.upd.mu.Unlock()
	if a.upd.helperOut.CompareAndSwap(true, false) {
		a.quitting.Store(false)
		a.setUpdatePhase("")
		a.upd.busy.Store(false)
	}
}

func (a *App) setUpdatePhase(phase string) {
	a.upd.mu.Lock()
	a.upd.phase, a.upd.done, a.upd.total = phase, 0, 0
	a.upd.mu.Unlock()
}

// ---- bindings ----

// CheckUpdate is the user's 지금 확인; unlike the scheduled check it reports failures.
func (a *App) CheckUpdate() (UpdateInfo, error) {
	if !a.updatesEnabled() {
		return a.updateInfo(), updateErr(codeUpdateDev, update.ErrDevBuild)
	}
	if err := a.runCheck(context.Background()); err != nil {
		code := codeUpdateCheck
		if errors.Is(err, update.ErrNoRelease) {
			code = codeUpdateNoRelease
		}
		return a.updateInfo(), updateErr(code, err)
	}
	return a.updateInfo(), nil
}

// DismissUpdateNotice forgets why the previous update attempt failed.
func (a *App) DismissUpdateNotice() {
	a.upd.mu.Lock()
	a.upd.notice = update.Status{}
	a.upd.mu.Unlock()
}

// OpenReleasePage opens the known release's page, or the latest one's, in the
// browser. Nothing without the exact release prefix is ever opened.
func (a *App) OpenReleasePage() error {
	url := update.ReleasesURL + "latest"
	a.upd.mu.Lock()
	if a.upd.release != nil {
		url = a.upd.release.PageURL()
	}
	a.upd.mu.Unlock()
	if !update.IsReleaseURL(url) {
		return updateErr(codeUpdateBadURL, errors.New("not a release page"))
	}
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx == nil {
		return wrapErr(vfs.E(vfs.CodeInternal, "the window is not ready"))
	}
	a.upd.deps.openURL(ctx, url)
	return nil
}

// ApplyUpdate downloads and verifies the installer, starts the helper, and quits;
// the helper does the rest (update.RunHelper). A second call while one runs returns
// at once. A failure at any stage leaves the app running.
func (a *App) ApplyUpdate() error {
	if a.goos != "windows" {
		return updateErr(codeUpdateUnsupported, update.ErrUnsupported)
	}
	if !a.upd.busy.CompareAndSwap(false, true) {
		return nil
	}
	if err := a.applyUpdate(); err != nil {
		a.setUpdatePhase("")
		a.upd.busy.Store(false)
		return err
	}
	// Still busy: the app is quitting. The flag makes closing quit, not hide.
	a.upd.helperOut.Store(true)
	a.quitFromTray()
	return nil
}

func (a *App) applyUpdate() (err error) {
	a.upd.mu.Lock()
	rel, available := a.upd.release, a.upd.available
	a.upd.mu.Unlock()
	if rel == nil || !available {
		return updateErr(codeUpdateNone, errors.New("no newer release is known"))
	}
	if !rel.Installable() {
		return updateErr(codeUpdateNoInstaller, update.ErrNoInstaller)
	}
	self, err := a.upd.deps.selfExe()
	if err != nil {
		return updateErr(codeUpdateHelper, err)
	}
	cfg := a.configDir
	if cfg != "" {
		if cfg, err = filepath.Abs(cfg); err != nil {
			return updateErr(codeUpdateHelper, err)
		}
	}
	dir, err := a.upd.deps.mkTemp()
	if err != nil {
		return updateErr(codeUpdateDownload, err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()

	a.setUpdatePhase(phaseDownloading)
	got, err := a.upd.deps.download(context.Background(), *rel, dir, func(done, total int64) {
		a.upd.mu.Lock()
		a.upd.done, a.upd.total = done, total
		a.upd.mu.Unlock()
	})
	if err != nil {
		return updateErr(downloadCode(err), err)
	}

	a.setUpdatePhase(phasePreparing)
	// A program cannot run from the file it replaces, so the helper is a copy.
	helper := filepath.Join(dir, update.HelperExe)
	if err := copyFile(self, helper); err != nil {
		return updateErr(codeUpdateHelper, err)
	}
	restart, err := a.stopDaemonOfThisProgram(self)
	if err != nil {
		if restart {
			// It accepted the shutdown, so sharing is going off.
			return a.restartDaemonAfter(self, cfg, codeUpdateDaemon, err)
		}
		return updateErr(codeUpdateDaemon, err)
	}
	failed := func(err error) error {
		if restart {
			return a.restartDaemonAfter(self, cfg, codeUpdateHelper, err)
		}
		return updateErr(codeUpdateHelper, err)
	}
	h := update.HelperArgs{PID: os.Getpid(), Installer: got.Path, SHA256: got.SHA256, App: self, ConfigDir: cfg, RestartDaemon: restart}
	abort, err := a.upd.deps.startHelper(helper, dir, h)
	if err != nil {
		return failed(err)
	}
	// Once this app has quit nothing could start it again, so make sure of the helper
	// first: an antivirus may remove it just as it starts.
	if err := a.upd.deps.waitMarker(dir, update.MarkerWait); err != nil {
		abort()
		return failed(err)
	}
	return nil
}

// restartDaemonAfter starts the stopped daemon again after a failure, and says so
// if that fails too, because sharing is then off.
func (a *App) restartDaemonAfter(self, cfg, code string, cause error) error {
	a.mu.Lock()
	a.be = nil
	a.mu.Unlock()
	if err := a.upd.deps.restartDaemon(self, cfg); err != nil {
		log.Printf("sugar-it: restart the daemon: %v", err)
		return updateErr(codeUpdateDaemonOff, fmt.Errorf("%v; restart the daemon: %w", cause, err))
	}
	return updateErr(code, cause)
}

func downloadCode(err error) string {
	switch {
	case errors.Is(err, update.ErrNoInstaller):
		return codeUpdateNoInstaller
	case errors.Is(err, update.ErrNoChecksum):
		return codeUpdateNoChecksum
	case errors.Is(err, update.ErrChecksumMismatch):
		return codeUpdateChecksum
	case errors.Is(err, update.ErrTooLarge):
		return codeUpdateTooLarge
	}
	return codeUpdateDownload
}

// stopDaemonOfThisProgram stops an external daemon that runs from this very file,
// such as a Run key's `sugar-it-desktop.exe daemon`: it locks the file against the
// installer. It reports whether it stopped one, so that the helper starts it again,
// which is also true with an error if the daemon accepted but has not exited. A daemon
// of another program, such as sugar-it.exe, locks nothing here and is left alone,
// and one that cannot be identified is treated the same way.
func (a *App) stopDaemonOfThisProgram(self string) (bool, error) {
	a.mu.Lock()
	inProcess := a.d != nil
	a.mu.Unlock()
	if inProcess {
		return false, nil // stops with the app
	}
	dir, err := a.resolvedConfigDir()
	if err != nil {
		return false, err
	}
	rt, err := control.ReadRuntime(dir)
	if err != nil || rt.PID == os.Getpid() {
		return false, nil
	}
	image, err := a.upd.deps.procImage(rt.PID)
	if err != nil {
		log.Printf("sugar-it: cannot tell which program daemon %d runs: %v", rt.PID, err)
		return false, nil
	}
	if !update.SameFile(image, self) {
		return false, nil
	}
	b, err := a.client()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		return false, fmt.Errorf("ask the daemon to stop: %w", err)
	}
	a.mu.Lock()
	a.be = nil // it is going away
	a.mu.Unlock()
	// The file stays locked until the process has really exited.
	if err := a.upd.deps.waitExit(rt.PID, daemon.Grace+5*time.Second); err != nil {
		return true, fmt.Errorf("wait for the daemon to stop: %w", err)
	}
	return true, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
