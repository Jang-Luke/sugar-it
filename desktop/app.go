package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"

	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
	"sugarintegration/internal/mount"
	"sugarintegration/internal/node"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/service"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

const (
	callTimeout  = 30 * time.Second
	mountTimeout = 5 * time.Minute // the OS may wait for the user to type a password
	tokenMark    = "{token}"
)

// Backend is what the App needs from the Control API; *control.Client is one.
type Backend interface {
	Status(ctx context.Context) (control.Status, error)
	Invite(ctx context.Context, ttl time.Duration) (string, error)
	JoinPeer(ctx context.Context, invite string) (node.PeerStatus, error)
	RemovePeer(ctx context.Context, name string) error
	RenameSelf(ctx context.Context, newName string) error
	ListShares(ctx context.Context, peer string) ([]peerapi.ShareInfo, error)
	AddShare(ctx context.Context, s shares.Spec) error
	RemoveShare(ctx context.Context, name string) error
	List(ctx context.Context, t control.Target) ([]vfs.Entry, error)
	Mkdir(ctx context.Context, t control.Target) error
	Remove(ctx context.Context, t control.Target, recursive bool) error
	Rename(ctx context.Context, t control.Target, newName string) error
	StartJob(ctx context.Context, r control.TransferRequest) (transfer.Job, error)
	Jobs(ctx context.Context) ([]transfer.Job, error)
	CancelJob(ctx context.Context, host, id string) error
	RetryJob(ctx context.Context, host, id string) error
	Token() string
	DAVAddr() string
}

// App is the Wails binding object: a thin layer over the Control API. The
// control token never leaves it except through the native token dialog.
type App struct {
	configDir string
	opts      daemon.Options // template for the in-process daemon; tests bind free ports

	// Seams for tests.
	discover     func(dir string) (Backend, error)
	winConnect   mount.Connector // the WNetUseConnectionW call on Windows
	readLimits   func() mount.WinLimits
	mountRun     func(ctx context.Context, p mount.Plan, out io.Writer) error
	svcRun       func(ctx context.Context, u service.Unit, out io.Writer) error
	svcUninstall func(ctx context.Context, u service.Unit, out io.Writer) error
	dialog       func(ctx context.Context, title, message string) error
	goos         string
	home         string // "" means the user's home directory
	exeDir       string // where to look for the sugar-it command; "" means beside this program

	mu       sync.Mutex
	ctx      context.Context
	be       Backend
	d        *daemon.Daemon
	startErr error
	failure  error // the in-process daemon's listener died
}

// NewApp makes an App for configDir ("" means the default directory).
func NewApp(configDir string) *App {
	return &App{
		configDir: configDir,
		discover: func(dir string) (Backend, error) {
			c, err := control.Discover(dir)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		mountRun:     mount.Run,
		winConnect:   mount.DefaultConnector,
		readLimits:   mount.ReadWinLimits,
		svcRun:       service.Install,
		svcUninstall: service.Uninstall,
		dialog: func(ctx context.Context, title, message string) error {
			_, err := wailsrt.MessageDialog(ctx, wailsrt.MessageDialogOptions{Type: wailsrt.InfoDialog, Title: title, Message: message})
			return err
		},
		goos: runtime.GOOS,
	}
}

// startup uses the daemon that runs for the config directory, or starts one
// inside this process. Failures are kept and reported by every later call.
func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ctx = ctx
	be, err := a.discover(a.configDir)
	if err == nil {
		a.be = be
		return
	}
	if !errors.Is(err, control.ErrUnavailable) {
		a.startErr = err
		return
	}
	o := a.opts
	o.ConfigDir, o.InProcess = a.configDir, true
	// Not ctx: the app stops the daemon itself, in shutdown.
	d, err := daemon.Start(context.Background(), o)
	if err != nil {
		a.startErr = err // another process holding the config directory lands here
		return
	}
	if be, err = a.discover(a.configDir); err != nil {
		d.Shutdown(context.Background())
		a.startErr = err
		return
	}
	a.d, a.be = d, be
	go func() {
		if err := d.Wait(); err != nil {
			a.mu.Lock()
			a.failure = err
			a.mu.Unlock()
		}
	}()
}

// shutdown stops the in-process daemon, if the app started one.
func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	d := a.d
	a.mu.Unlock()
	if d != nil {
		d.Shutdown(ctx)
	}
}

// ---- errors ----

// apiError is what a binding returns on failure: Wails hands err.Error() to
// the front end, so that is the JSON {"code","message"}.
type apiError struct{ e vfs.Error }

func (a apiError) Error() string {
	b, _ := json.Marshal(a.e)
	return string(b)
}

// wrapErr turns any error into an apiError carrying the spec section 6 code.
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	var ve *vfs.Error
	if errors.As(err, &ve) {
		msg = strings.TrimPrefix(msg, ve.Code+": ")
	}
	return apiError{vfs.Error{Code: vfs.CodeOf(err), Message: msg}}
}

// client returns the backend, finding the daemon again if it was lost.
func (a *App) client() (Backend, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.startErr != nil {
		return nil, a.startErr
	}
	if a.failure != nil {
		return nil, vfs.E(vfs.CodeInternal, "the sharing service stopped: %v", a.failure)
	}
	if a.be != nil {
		return a.be, nil
	}
	be, err := a.discover(a.configDir)
	if err != nil {
		return nil, err
	}
	a.be = be
	return be, nil
}

// finish drops a backend whose daemon is gone, so that the next call looks for
// a restarted one, and converts err for the front end.
func (a *App) finish(err error) error {
	if errors.Is(err, control.ErrUnavailable) {
		a.mu.Lock()
		a.be = nil
		a.mu.Unlock()
	}
	return wrapErr(err)
}

// call runs f against the backend with a deadline, so that nothing hangs.
func call[T any](a *App, timeout time.Duration, f func(ctx context.Context, b Backend) (T, error)) (T, error) {
	var zero T
	b, err := a.client()
	if err != nil {
		return zero, wrapErr(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	v, err := f(ctx, b)
	if err != nil {
		return zero, a.finish(err)
	}
	return v, nil
}

func do(a *App, f func(ctx context.Context, b Backend) error) error {
	_, err := call(a, callTimeout, func(ctx context.Context, b Backend) (struct{}, error) { return struct{}{}, f(ctx, b) })
	return err
}

// ---- state that needs no daemon ----

// AppState is what the window needs even when the daemon cannot be reached.
type AppState struct {
	InProcess  bool   // closing the app stops sharing
	StartError string // the daemon could neither be found nor started
	Failure    string // the in-process daemon's listener died
	Platform   string // GOOS
}

func (a *App) AppState() AppState {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := AppState{InProcess: a.d != nil, Platform: a.goos}
	if a.startErr != nil {
		s.StartError = a.startErr.Error()
	}
	if a.failure != nil {
		s.Failure = a.failure.Error()
	}
	return s
}

// ---- bindings ----

func (a *App) Status() (control.Status, error) {
	return call(a, callTimeout, func(ctx context.Context, b Backend) (control.Status, error) { return b.Status(ctx) })
}

// Invite makes an invitation valid for ttlSeconds (0 means the default).
func (a *App) Invite(ttlSeconds int64) (string, error) {
	return call(a, callTimeout, func(ctx context.Context, b Backend) (string, error) {
		return b.Invite(ctx, time.Duration(ttlSeconds)*time.Second)
	})
}

func (a *App) JoinPeer(invite string) (node.PeerStatus, error) {
	return call(a, callTimeout, func(ctx context.Context, b Backend) (node.PeerStatus, error) { return b.JoinPeer(ctx, invite) })
}

func (a *App) RemovePeer(name string) error {
	return do(a, func(ctx context.Context, b Backend) error { return b.RemovePeer(ctx, name) })
}

// RenameSelf changes this host's name; only the host itself can.
func (a *App) RenameSelf(newName string) error {
	return do(a, func(ctx context.Context, b Backend) error { return b.RenameSelf(ctx, newName) })
}

func (a *App) ListShares(peer string) ([]peerapi.ShareInfo, error) {
	return call(a, callTimeout, func(ctx context.Context, b Backend) ([]peerapi.ShareInfo, error) {
		return b.ListShares(ctx, peer)
	})
}

func (a *App) AddShare(name, path string, readOnly bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return wrapErr(vfs.E(vfs.CodeInvalidPath, "%v", err))
	}
	return do(a, func(ctx context.Context, b Backend) error {
		return b.AddShare(ctx, shares.Spec{Name: name, Path: abs, ReadOnly: readOnly})
	})
}

func (a *App) RemoveShare(name string) error {
	return do(a, func(ctx context.Context, b Backend) error { return b.RemoveShare(ctx, name) })
}

func (a *App) List(peer, share, path string) ([]vfs.Entry, error) {
	return call(a, callTimeout, func(ctx context.Context, b Backend) ([]vfs.Entry, error) {
		return b.List(ctx, control.Target{Peer: peer, Share: share, Path: path})
	})
}

func (a *App) Mkdir(peer, share, path string) error {
	return do(a, func(ctx context.Context, b Backend) error {
		return b.Mkdir(ctx, control.Target{Peer: peer, Share: share, Path: path})
	})
}

func (a *App) Remove(peer, share, path string, recursive bool) error {
	return do(a, func(ctx context.Context, b Backend) error {
		return b.Remove(ctx, control.Target{Peer: peer, Share: share, Path: path}, recursive)
	})
}

func (a *App) Rename(peer, share, path, newName string) error {
	return do(a, func(ctx context.Context, b Backend) error {
		return b.Rename(ctx, control.Target{Peer: peer, Share: share, Path: path}, newName)
	})
}

// StartJob starts a copy or move. src and dst are "peer:share/path".
func (a *App) StartJob(op, src, dst, onConflict string) (transfer.Job, error) {
	return call(a, callTimeout, func(ctx context.Context, b Backend) (transfer.Job, error) {
		return b.StartJob(ctx, control.TransferRequest{Op: op, Src: src, Dst: dst, OnConflict: onConflict})
	})
}

func (a *App) Jobs() ([]transfer.Job, error) {
	return call(a, callTimeout, func(ctx context.Context, b Backend) ([]transfer.Job, error) { return b.Jobs(ctx) })
}

func (a *App) CancelJob(host, id string) error {
	return do(a, func(ctx context.Context, b Backend) error { return b.CancelJob(ctx, host, id) })
}

func (a *App) RetryJob(host, id string) error {
	return do(a, func(ctx context.Context, b Backend) error { return b.RetryJob(ctx, host, id) })
}

// PickFolder opens the native folder chooser. "" means the user cancelled.
func (a *App) PickFolder(title string) (string, error) {
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx == nil {
		return "", wrapErr(vfs.E(vfs.CodeInternal, "the window is not ready"))
	}
	p, err := wailsrt.OpenDirectoryDialog(ctx, wailsrt.OpenDialogOptions{Title: title})
	return p, wrapErr(err)
}

// ---- mount and service ----

// MountResult is what the front end may know about a mount. It never has the token.
type MountResult struct {
	Notes      []string
	Output     string
	NeedsToken bool   // macOS asks the user to type the token: see ShowMountToken
	Drive      string // the drive Windows assigned
	// Blocked: Windows refuses HTTP Basic until an administrator runs
	// AdminCommands. It is a result, not an error: nothing is broken.
	Blocked       bool
	AdminCommands []string
}

// redact removes the control token from text that goes back to the front end.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

func (a *App) winLimits() mount.WinLimits {
	if a.goos != "windows" {
		return mount.WinLimits{}
	}
	return a.readLimits()
}

// runnerFunc adapts the injectable mountRun to mount.Runner for plans that
// run a command; the Windows plan never does.
type planRunner struct {
	a    *App
	plan mount.Plan
}

func (r planRunner) Run(ctx context.Context, _ []string, _ string, out io.Writer) error {
	return r.a.mountRun(ctx, r.plan, out)
}

// Mount mounts the mesh as a network drive. On Linux the token goes to the
// command on stdin; on Windows it is an argument of WNetUseConnectionW; on
// macOS the user types it (ShowMountToken).
func (a *App) Mount(drive string) (MountResult, error) {
	return call(a, mountTimeout, func(ctx context.Context, b Backend) (MountResult, error) {
		token := b.Token()
		plan, err := mount.MountPlan(a.goos, b.DAVAddr(), token, drive, a.winLimits())
		var blocked *mount.BasicAuthBlockedError
		if errors.As(err, &blocked) {
			return MountResult{Blocked: true, AdminCommands: blocked.Commands}, nil
		}
		if err != nil {
			return MountResult{}, errors.New(redact(err.Error(), token))
		}
		var out bytes.Buffer
		assigned, err := mount.RunMount(ctx, planRunner{a, plan}, a.winConnect, plan, token, &out)
		if err != nil {
			return MountResult{Output: redact(out.String(), token)}, errors.New(redact(err.Error(), token))
		}
		return MountResult{Notes: plan.Notes, Output: redact(out.String(), token), NeedsToken: plan.Stdin == "" && plan.Windows == nil, Drive: assigned}, nil
	})
}

func (a *App) Unmount(drive string) (MountResult, error) {
	return call(a, mountTimeout, func(ctx context.Context, b Backend) (MountResult, error) {
		plan, err := mount.UnmountPlan(a.goos, b.DAVAddr(), drive)
		if err != nil {
			return MountResult{}, err
		}
		var out bytes.Buffer
		err = a.mountRun(ctx, plan, &out)
		return MountResult{Notes: plan.Notes, Output: redact(out.String(), b.Token())}, err
	})
}

// ShowMountToken shows the token in a native dialog, where "{token}" in message
// stands for it. It is the only way the token leaves the app, and it goes to
// the user's screen, not to the front end. Only macOS needs it.
func (a *App) ShowMountToken(title, message string) error {
	if a.goos != "darwin" {
		return wrapErr(vfs.E(vfs.CodeInvalidRequest, "the mount gets the token itself on this system"))
	}
	b, err := a.client()
	if err != nil {
		return wrapErr(err)
	}
	if !strings.Contains(message, tokenMark) {
		message += "\n\n" + tokenMark
	}
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return wrapErr(a.dialog(ctx, title, strings.ReplaceAll(message, tokenMark, b.Token())))
}

// ServiceResult is the output of the install or uninstall commands.
type ServiceResult struct{ Output string }

func cliName() string {
	if runtime.GOOS == "windows" {
		return "sugar-it.exe"
	}
	return "sugar-it"
}

// serviceTarget is the program a service runs: the sugar-it command beside this
// program if there is one, else this program, whose first argument daemon starts
// no window (see runDaemon). PATH is not searched: an app started from the Finder
// does not have the user's, and a package carries one or the other.
func (a *App) serviceTarget() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := a.exeDir
	if dir == "" {
		dir = filepath.Dir(self)
	}
	p := filepath.Join(dir, cliName())
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p, nil
	}
	return self, nil
}

// InstallService installs the per-user background service for the config directory.
func (a *App) InstallService() (ServiceResult, error) {
	exe, err := a.serviceTarget()
	if err != nil {
		return ServiceResult{}, wrapErr(err)
	}
	home := a.home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return ServiceResult{}, wrapErr(err)
		}
	}
	cfg := a.configDir
	if cfg != "" {
		if cfg, err = filepath.Abs(cfg); err != nil {
			return ServiceResult{}, wrapErr(err)
		}
	}
	u, err := service.Plan(a.goos, exe, home, cfg)
	if err != nil {
		return ServiceResult{}, wrapErr(vfs.E(vfs.CodeInvalidRequest, "%v", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	defer cancel()
	var out bytes.Buffer
	err = a.svcRun(ctx, u, &out)
	return ServiceResult{Output: out.String()}, wrapErr(err)
}

// UninstallService stops and removes the per-user background service.
func (a *App) UninstallService() (ServiceResult, error) {
	home := a.home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return ServiceResult{}, wrapErr(err)
		}
	}
	u, err := service.UninstallPlan(a.goos, home)
	if err != nil {
		return ServiceResult{}, wrapErr(vfs.E(vfs.CodeInvalidRequest, "%v", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	defer cancel()
	var out bytes.Buffer
	err = a.svcUninstall(ctx, u, &out)
	return ServiceResult{Output: out.String()}, wrapErr(err)
}
