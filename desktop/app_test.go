package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
	"sugarintegration/internal/mount"
	"sugarintegration/internal/service"
	"sugarintegration/internal/vfs"
)

// loopback makes a daemon (and the app's own) bind free local ports.
var loopback = daemon.Options{ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0", DAVAddr: "127.0.0.1:0"}

func startDaemon(t *testing.T, dir string) *daemon.Daemon {
	t.Helper()
	o := loopback
	o.ConfigDir = dir
	d, err := daemon.Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Shutdown(context.Background()) })
	return d
}

func newApp(t *testing.T, dir string) *App {
	t.Helper()
	a := NewApp(dir)
	a.opts = loopback
	a.tray = nil // never a real tray in tests
	return a
}

// parseErr decodes the JSON that a binding error carries to the front end.
func parseErr(t *testing.T, err error) (code, msg string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	var v struct{ Code, Message string }
	if json.Unmarshal([]byte(err.Error()), &v) != nil || v.Code == "" {
		t.Fatalf("error %q is not {code,message} JSON", err.Error())
	}
	return v.Code, v.Message
}

func TestStartupUsesRunningDaemon(t *testing.T) {
	dir := t.TempDir()
	startDaemon(t, dir)
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	if a.d != nil {
		t.Fatal("the app started a daemon of its own although one was running")
	}
	st, err := a.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.InProcess {
		t.Error("InProcess = true for an external daemon")
	}
	if s := a.AppState(); s.InProcess || s.StartError != "" {
		t.Errorf("AppState = %+v", s)
	}
}

func TestStartupStartsInProcess(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	a.startup(context.Background())
	if a.d == nil {
		t.Fatalf("no in-process daemon; AppState = %+v", a.AppState())
	}
	st, err := a.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.InProcess || st.ID == "" {
		t.Errorf("Status = %+v", st)
	}
	if !a.AppState().InProcess {
		t.Error("AppState.InProcess = false")
	}
	addrs := []string{a.d.ControlAddr(), a.d.PeerAddr(), a.d.DAVAddr()}
	a.shutdown(context.Background())
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("port %s still held after shutdown: %v", addr, err)
			continue
		}
		ln.Close()
	}
	if _, err := os.Stat(filepath.Join(dir, control.RuntimeFile)); !os.IsNotExist(err) {
		t.Errorf("runtime.json left behind: %v", err)
	}
}

// Another process holding the config directory's lock is reported, not fatal.
func TestStartupSurfacesLockHeldElsewhere(t *testing.T) {
	dir := t.TempDir()
	startDaemon(t, dir)
	// Hide the daemon from Discover while it still holds the lock.
	if err := os.Remove(filepath.Join(dir, control.RuntimeFile)); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	if a.AppState().StartError == "" {
		t.Fatal("no start error recorded")
	}
	_, err := a.Status()
	code, msg := parseErr(t, err)
	if code != vfs.CodeInternal || !strings.Contains(msg, "already running") {
		t.Errorf("Status error = %s: %s", code, msg)
	}
}

type failingBackend struct{ Backend }

func (failingBackend) Mkdir(context.Context, control.Target) error {
	return vfs.E(vfs.CodeReadOnly, "share %q is read-only", "docs")
}

func TestErrorsSerializedWithCode(t *testing.T) {
	a := newApp(t, t.TempDir())
	a.be = failingBackend{}
	code, msg := parseErr(t, a.Mkdir("local", "docs", "x"))
	if code != "read_only" || !strings.Contains(msg, "read-only") {
		t.Errorf("got %q %q", code, msg)
	}
	// Wrapped errors keep their code too.
	if got := wrapErr(errors.Join(errors.New("x"), vfs.E(vfs.CodeChanged, "moved"))); got == nil {
		t.Fatal("nil")
	} else if c, _ := parseErr(t, got); c != "changed" {
		t.Errorf("wrapped code = %q", c)
	}
	// Anything else is internal.
	if c, _ := parseErr(t, wrapErr(errors.New("boom"))); c != "internal" {
		t.Errorf("plain error code = %q", c)
	}
	if wrapErr(nil) != nil {
		t.Error("wrapErr(nil) != nil")
	}
}

func TestDaemonDiesMidSession(t *testing.T) {
	dir := t.TempDir()
	d := startDaemon(t, dir)
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	if _, err := a.Status(); err != nil {
		t.Fatal(err)
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { _, err := a.Jobs(); done <- err }()
	select {
	case err := <-done:
		code, msg := parseErr(t, err)
		if code == vfs.CodePeerOffline || code != vfs.CodeInternal || !strings.Contains(msg, "no sugar-it daemon") {
			t.Errorf("got %s: %s", code, msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("call hung after the daemon died")
	}

	// A restarted daemon is found again without restarting the app.
	startDaemon(t, dir)
	if _, err := a.Status(); err != nil {
		t.Errorf("no reconnect after the daemon came back: %v", err)
	}
}

func TestBindingsPassArguments(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())

	share := t.TempDir()
	os.WriteFile(filepath.Join(share, "한글 파일.txt"), []byte("hi"), 0o600)
	if err := a.AddShare("docs", share, false); err != nil {
		t.Fatal(err)
	}
	infos, err := a.ListShares("local")
	if err != nil || len(infos) != 1 || infos[0].Name != "docs" {
		t.Fatalf("shares = %+v, %v", infos, err)
	}
	if err := a.Mkdir("local", "docs", "새 폴더"); err != nil {
		t.Fatal(err)
	}
	es, err := a.List("local", "docs", "")
	if err != nil || len(es) != 2 {
		t.Fatalf("list = %+v, %v", es, err)
	}
	if err := a.Rename("local", "docs", "새 폴더", "other"); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove("local", "docs", "other", true); err != nil {
		t.Fatal(err)
	}
	// A copy between two local paths runs through the job manager.
	j, err := a.StartJob("copy", "local:docs/한글 파일.txt", "local:docs/copy.txt", "fail")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		js, err := a.Jobs()
		if err != nil {
			t.Fatal(err)
		}
		if len(js) == 1 && js[0].ID == j.ID && js[0].State == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %+v", js)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, err := os.ReadFile(filepath.Join(share, "copy.txt"))
	if err != nil || string(b) != "hi" {
		t.Errorf("copy = %q, %v", b, err)
	}
	if err := a.RemoveShare("docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invite(0); err != nil {
		t.Errorf("Invite: %v", err)
	}
	if err := a.RemovePeer("local"); err == nil {
		t.Error("removing self succeeded")
	} else if c, _ := parseErr(t, err); c == "" {
		t.Error("no code")
	}
}

func TestTokenStaysInTheApp(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	token, err := config.ControlToken(dir)
	if err != nil || token == "" {
		t.Fatal("no token", err)
	}

	var calls []mount.Plan
	var shown []string
	a.mountRun = func(_ context.Context, p mount.Plan, out io.Writer) error {
		calls = append(calls, p)
		out.Write([]byte("echo " + token + "\n")) // a tool that leaks the token into its output
		return nil
	}
	a.dialog = func(_ context.Context, title, msg string) error { shown = append(shown, msg); return nil }
	var winArgs []mount.ConnectArgs
	a.winConnect = func(c mount.ConnectArgs) (string, error) {
		winArgs = append(winArgs, c)
		return "Y:", nil
	}

	var all []any
	st, _ := a.Status()
	all = append(all, st, a.AppState())

	for _, goos := range []string{"linux", "darwin", "windows"} {
		a.goos = goos
		calls = nil
		res, err := a.Mount("")
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		all = append(all, res)
		if goos == "windows" {
			if len(calls) != 0 || len(winArgs) != 1 || winArgs[0].Password != token || res.Drive != "Y:" {
				t.Fatalf("windows: runs %d, connects %+v, drive %q", len(calls), winArgs, res.Drive)
			}
			if res.NeedsToken {
				t.Error("windows needs no token dialog")
			}
			continue
		}
		if len(calls) != 1 {
			t.Fatalf("%s: %d mount runs", goos, len(calls))
		}
		p := calls[0]
		if strings.Contains(strings.Join(p.Argv, " "), token) || strings.Contains(strings.Join(p.Notes, "\n"), token) {
			t.Errorf("%s: token in argv or notes", goos)
		}
		if goos == "linux" && p.Stdin != token+"\n" {
			t.Errorf("linux stdin = %q", p.Stdin)
		}
		if goos != "linux" && p.Stdin != "" {
			t.Errorf("%s: stdin set", goos)
		}
		if res.NeedsToken != (goos == "darwin") {
			t.Errorf("%s: NeedsToken = %v", goos, res.NeedsToken)
		}
	}
	b, _ := json.Marshal(all)
	if strings.Contains(string(b), token) {
		t.Errorf("token reachable from binding results: %s", b)
	}

	// Only the native dialog gets the token.
	a.goos = "darwin"
	if err := a.ShowMountToken("t", "토큰: {token}"); err != nil {
		t.Fatal(err)
	}
	if len(shown) != 1 || !strings.Contains(shown[0], token) {
		t.Errorf("dialog = %q", shown)
	}
	// Not offered where the token goes to the command or the API itself.
	for _, goos := range []string{"linux", "windows"} {
		a.goos = goos
		if err := a.ShowMountToken("t", "토큰: {token}"); err == nil {
			t.Errorf("token dialog opened on %s", goos)
		}
	}
}

func TestMountErrorsDoNotLeakToken(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	token, _ := config.ControlToken(dir)
	a.goos = "linux"
	a.mountRun = func(_ context.Context, _ mount.Plan, out io.Writer) error {
		return errors.New("gio failed with " + token)
	}
	_, err := a.Mount("")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("token in error %q", err)
	}
}

func TestInstallServicePlan(t *testing.T) {
	dir := t.TempDir()
	bin := t.TempDir()
	cli := filepath.Join(bin, cliName())
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, dir)
	a.exeDir = bin
	a.goos = "linux"
	a.home = t.TempDir()
	var got service.Unit
	a.svcRun = func(_ context.Context, u service.Unit, out io.Writer) error {
		got = u
		out.Write([]byte("installed\n"))
		return nil
	}
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	res, err := a.InstallService()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Content, `"`+cli+`" daemon --foreground --config-dir "`+dir+`"`) {
		t.Errorf("unit does not run the CLI daemon on this config dir:\n%s", got.Content)
	}
	if !strings.Contains(res.Output, "installed") {
		t.Errorf("result = %+v", res)
	}

}

// The desktop package has no sugar-it command, and a Finder-launched app has no
// useful PATH: with nothing beside it, the service runs this program itself.
func TestInstallServiceTargetsThisProgramWithoutCLIBeside(t *testing.T) {
	onPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(onPath, cliName()), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath)
	dir := t.TempDir()
	a := newApp(t, dir)
	a.exeDir = t.TempDir()
	a.goos = "linux"
	a.home = t.TempDir()
	var got service.Unit
	a.svcRun = func(_ context.Context, u service.Unit, _ io.Writer) error {
		got = u
		return nil
	}
	if _, err := a.InstallService(); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := `"` + self + `" daemon --foreground --config-dir "` + dir + `"`
	if !strings.Contains(got.Content, want) {
		t.Errorf("unit does not run %s:\n%s", want, got.Content)
	}
}

func TestDaemonArgumentRunsTheCLI(t *testing.T) {
	var out, errb bytes.Buffer
	code, handled := runDaemon([]string{"daemon", "--help"}, &out, &errb)
	if !handled || code != 0 || !strings.Contains(out.String(), "Run the sugar-it daemon") {
		t.Errorf("handled %v, code %d, stdout %q, stderr %q", handled, code, out.String(), errb.String())
	}
	// The detached child of a background start is this program with these
	// arguments; they must reach the CLI too, not open the window.
	out.Reset()
	code, handled = runDaemon([]string{"daemon", "--foreground", "--config-dir", "x", "--help"}, &out, &errb)
	if !handled || code != 0 || !strings.Contains(out.String(), "--foreground") {
		t.Errorf("daemon --foreground: handled %v, code %d, stdout %q", handled, code, out.String())
	}
	for _, args := range [][]string{nil, {"--config-dir", "x"}, {"--config-dir", "daemon"}} {
		if _, handled := runDaemon(args, &out, &errb); handled {
			t.Errorf("%v must open the window", args)
		}
	}
}

func TestRenameSelfBinding(t *testing.T) {
	a := newApp(t, t.TempDir())
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	if err := a.RenameSelf("new-name"); err != nil {
		t.Fatal(err)
	}
	st, err := a.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "new-name" {
		t.Errorf("name = %q", st.Name)
	}
	if code, _ := parseErr(t, a.RenameSelf("Not Valid")); code != vfs.CodeInvalidRequest {
		t.Errorf("code = %s", code)
	}
}

func TestUninstallServicePlan(t *testing.T) {
	a := newApp(t, t.TempDir())
	a.goos = "linux"
	a.home = t.TempDir()
	var got service.Unit
	a.svcUninstall = func(_ context.Context, u service.Unit, out io.Writer) error {
		got = u
		out.Write([]byte("removed\n"))
		return nil
	}
	res, err := a.UninstallService()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(a.home, ".config", "systemd", "user", "sugar-it.service"); got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}
	if !strings.Contains(res.Output, "removed") {
		t.Errorf("result = %+v", res)
	}
	a.goos = "plan9"
	if code, _ := parseErr(t, func() error { _, err := a.UninstallService(); return err }()); code != vfs.CodeInvalidRequest {
		t.Errorf("code = %s", code)
	}
}

func TestMountBlockedByBasicAuthIsAResult(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	a.goos = "windows"
	a.readLimits = func() mount.WinLimits {
		return mount.WinLimits{Known: true, BasicAuthLevel: 1, FileSizeLimit: 4294967295}
	}
	a.winConnect = func(mount.ConnectArgs) (string, error) { t.Fatal("must not connect"); return "", nil }
	res, err := a.Mount("")
	if err != nil {
		t.Fatalf("blocked is a result, not an error: %v", err)
	}
	if !res.Blocked || len(res.AdminCommands) != 2 || !strings.HasPrefix(res.AdminCommands[0], "reg add ") {
		t.Fatalf("%+v", res)
	}
}

func TestMountConnectFailureStaysAnError(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	token, _ := config.ControlToken(dir)
	a.goos = "windows"
	a.readLimits = func() mount.WinLimits {
		return mount.WinLimits{Known: true, BasicAuthLevel: 2, FileSizeLimit: 4294967295}
	}
	a.winConnect = func(c mount.ConnectArgs) (string, error) { return "", errors.New("boom " + c.Password) }
	res, err := a.Mount("Z:")
	if err == nil || res.Blocked || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("%+v %v", res, err)
	}
}

type okRunner struct{}

func (okRunner) Run(context.Context, []string, string, io.Writer) error { return nil }

// The real mount.Run prints a plan's notes; they must reach the window once
// (in Notes), not again in Output, and macOS must not point at a terminal.
func TestMountNotesAppearOnceAndNameTheAppsToken(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	a.mountRun = func(ctx context.Context, p mount.Plan, out io.Writer) error {
		return mount.RunWith(ctx, okRunner{}, p, out)
	}
	for _, goos := range []string{"linux", "darwin"} {
		a.goos = goos
		res, err := a.Mount("")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Notes) == 0 {
			t.Fatalf("%s: no notes", goos)
		}
		if strings.Contains(res.Output, res.Notes[0]) {
			t.Errorf("%s: notes printed twice: output %q", goos, res.Output)
		}
		if strings.Contains(strings.Join(res.Notes, "\n"), "printed below") {
			t.Errorf("%s: note points at a terminal: %q", goos, res.Notes)
		}
		u, err := a.Unmount("")
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range u.Notes {
			if strings.Contains(u.Output, n) {
				t.Errorf("%s: unmount notes printed twice: %q", goos, u.Output)
			}
		}
	}
}

func windowsUninstallApp(t *testing.T, a *App) {
	t.Helper()
	a.goos = "windows"
	a.home = t.TempDir()
	a.svcUninstall = func(_ context.Context, _ service.Unit, out io.Writer) error {
		out.Write([]byte("Removed the logon entry\n"))
		return nil
	}
}

func TestWindowsUninstallStopsAnExternalDaemon(t *testing.T) {
	dir := t.TempDir()
	startDaemon(t, dir)
	a := newApp(t, dir)
	a.startup(context.Background())
	windowsUninstallApp(t, a)
	res, err := a.UninstallService()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "Asked the running daemon to stop") {
		t.Errorf("output %q", res.Output)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := control.Discover(dir); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the external daemon is still running")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWindowsUninstallLeavesTheAppsOwnDaemon(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	windowsUninstallApp(t, a)
	res, err := a.UninstallService()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "when the app quits") {
		t.Errorf("output %q", res.Output)
	}
	if _, err := a.Status(); err != nil {
		t.Errorf("the app's daemon must keep running: %v", err)
	}
}

func TestLinuxUninstallDoesNotTouchTheDaemon(t *testing.T) {
	dir := t.TempDir()
	startDaemon(t, dir)
	a := newApp(t, dir)
	a.startup(context.Background())
	a.goos = "linux"
	a.home = t.TempDir()
	a.svcUninstall = func(context.Context, service.Unit, io.Writer) error { return nil }
	if _, err := a.UninstallService(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Status(); err != nil {
		t.Errorf("the service manager stops its own process; the app must not: %v", err)
	}
}
