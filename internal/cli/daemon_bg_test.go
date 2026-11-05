package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"sugarintegration/internal/cli"
	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
)

// The background start re-executes os.Executable(), which in these tests is the
// test binary: it answers a first argument of daemon like the real program.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

type bgHost struct {
	*host
	ctl, dav string
}

// newBG prepares a config dir and registers a cleanup that stops a daemon
// started on it, by its config dir only.
func newBG(t *testing.T, name string) *bgHost {
	t.Helper()
	h := &bgHost{host: newConfig(t, name)}
	h.ctl, h.dav = freeAddr(t), freeAddr(t)
	t.Cleanup(func() { h.run("stop") })
	return h
}

func (h *bgHost) start(extra ...string) (int, string, string) {
	h.t.Helper()
	args := append([]string{"daemon", "--listen", h.peer, "--control", h.ctl, "--dav", h.dav}, extra...)
	return h.run(args...)
}

func readRT(t *testing.T, dir string) control.Runtime {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, control.RuntimeFile))
	if err != nil {
		t.Fatal(err)
	}
	var rt control.Runtime
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestDaemonBackgroundStartsAndStops(t *testing.T) {
	h := newBG(t, "bg")
	start := time.Now()
	code, out, errs := h.start("--advertise", "10.1.2.3:4567")
	if code != 0 {
		t.Fatalf("daemon: exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	t.Logf("returned after %v: %s", time.Since(start), out)
	// The command returned while the daemon answers, and a second call sees it.
	if got := h.ok("status", "--json"); !strings.Contains(got, `"Name"`) {
		t.Fatalf("status: %s", got)
	}
	// Flags passed through to the child.
	rt := readRT(t, h.dir)
	if rt.ControlAddr != h.ctl || rt.DAVAddr != h.dav || rt.PeerAddr != h.peer {
		t.Errorf("runtime.json %+v, want control %s dav %s peer %s", rt, h.ctl, h.dav, h.peer)
	}
	if got := mustCfg(t, h.dir).Get().AdvertiseAddrs; len(got) != 1 || got[0] != "10.1.2.3:4567" {
		t.Errorf("advertise not passed on: %v", got)
	}
	// One line with the addresses, the log and how to stop; never the token.
	logPath := filepath.Join(h.dir, "daemon.log")
	for _, want := range []string{h.ctl, h.dav, h.peer, logPath, "sugar-it stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("start line lacks %q: %q", want, out)
		}
	}
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n"); n != 0 {
		t.Errorf("start printed %d lines: %q", n+1, out)
	}
	tok, _ := os.ReadFile(filepath.Join(h.dir, "control.token"))
	logb, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logb), "daemon running") {
		t.Errorf("daemon.log lacks the running line:\n%s", logb)
	}
	if len(bytes.TrimSpace(tok)) > 0 && (strings.Contains(out, strings.TrimSpace(string(tok))) || strings.Contains(string(logb), strings.TrimSpace(string(tok)))) {
		t.Error("the control token leaked")
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(logPath); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("daemon.log mode %v, %v", fi.Mode(), err)
		}
	}

	// A second start fails and leaves the first running.
	code, out2, err2 := h.start()
	if code != 1 || !strings.Contains(err2, "already running") {
		t.Errorf("second start: exit %d stdout %q stderr %q", code, out2, err2)
	}
	if got := readRT(t, h.dir); got != rt {
		t.Errorf("second start changed runtime.json: %+v", got)
	}
	h.ok("status")
	if after, _ := os.ReadFile(logPath); !bytes.Equal(after, logb) {
		t.Errorf("second start touched daemon.log:\n%s", after)
	}

	// stop: then status says not running, the ports are free, runtime.json is gone.
	if out := h.ok("stop"); !strings.Contains(out, "stopped") {
		t.Errorf("stop printed %q", out)
	}
	code, _, errs = h.run("status")
	if code != 1 || !strings.Contains(errs, "no sugar-it daemon is running") {
		t.Errorf("status after stop: exit %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(h.dir, control.RuntimeFile)); !os.IsNotExist(err) {
		t.Errorf("runtime.json after stop: %v", err)
	}
	for _, a := range []string{h.ctl, h.dav, h.peer} {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			t.Errorf("port %s not free after stop: %v", a, err)
			continue
		}
		ln.Close()
	}
	// ...and it starts again on the same directory, appending to the log.
	if code, out, errs := h.start(); code != 0 {
		t.Fatalf("restart: exit %d %s %s", code, out, errs)
	}
	if after, _ := os.ReadFile(logPath); !bytes.HasPrefix(after, logb) || bytes.Count(after, []byte("daemon running")) != 2 {
		t.Errorf("daemon.log was not appended:\n%s", after)
	}
}

func TestDaemonBackgroundBindConflict(t *testing.T) {
	h := newBG(t, "bgc")
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	h.ctl = busy.Addr().String()
	code, out, errs := h.start()
	if code != 1 {
		t.Fatalf("exit %d, stdout %q stderr %q", code, out, errs)
	}
	if !strings.Contains(errs, "cannot listen on "+h.ctl) {
		t.Errorf("stderr lacks the log tail: %q", errs)
	}
	if out != "" {
		t.Errorf("stdout %q on a failed start", out)
	}
	if code, _, _ := h.run("status"); code != 1 {
		t.Errorf("a daemon runs after the failed start")
	}
}

func TestStopWithoutDaemon(t *testing.T) {
	h := newConfig(t, "none")
	code, out, errs := h.run("stop")
	if code != 1 || out != "" || !strings.Contains(errs, "no sugar-it daemon is running") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errs)
	}
}

func TestForegroundReturnsAfterStop(t *testing.T) {
	h := newBG(t, "fg")
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() {
		done <- cli.Execute([]string{"--config-dir", h.dir, "daemon", "--foreground", "--listen", h.peer, "--control", h.ctl, "--dav", h.dav}, &bytes.Buffer{}, &errb)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := control.Discover(h.dir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never came up: %s", errb.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.ok("stop")
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("foreground daemon exit %d after stop", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("foreground daemon did not return after stop")
	}
	if _, err := os.Stat(filepath.Join(h.dir, control.RuntimeFile)); !os.IsNotExist(err) {
		t.Errorf("runtime.json after stop: %v", err)
	}
}

func TestStopRefusedForInProcessDaemon(t *testing.T) {
	h := newConfig(t, "inproc")
	d, err := daemon.Start(bg, daemon.Options{ConfigDir: h.dir, InProcess: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		d.Shutdown(ctx)
	}()
	code, _, errs := h.run("stop")
	if code != 1 || !strings.Contains(errs, "invalid_request") || !strings.Contains(errs, "quit the app") {
		t.Errorf("exit %d stderr %q", code, errs)
	}
	h.ok("status")
}

func TestRotateLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "daemon.log")
	os.WriteFile(p, []byte("small"), 0o600)
	if err := cli.RotateLog(p, 10); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "small" {
		t.Errorf("a small log was rotated")
	}
	os.WriteFile(p+".1", []byte("older"), 0o600)
	os.WriteFile(p, []byte("0123456789ABC"), 0o600)
	if err := cli.RotateLog(p, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("big log still in place: %v", err)
	}
	if b, _ := os.ReadFile(p + ".1"); string(b) != "0123456789ABC" {
		t.Errorf("daemon.log.1 = %q", b)
	}
	if err := cli.RotateLog(filepath.Join(dir, "missing.log"), 10); err != nil {
		t.Errorf("missing log: %v", err)
	}
}

func mustCfg(t *testing.T, dir string) *config.Store {
	t.Helper()
	c, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The detached child must be started with daemon first (the desktop binary
// takes only that for the CLI) and the same flags.
func TestChildArgs(t *testing.T) {
	got := cli.ChildArgs("/c d", "1:1", "2:2", "3:3", []string{"a:1", "b:2"})
	want := []string{"daemon", "--foreground", "--config-dir", "/c d", "--listen", "1:1", "--control", "2:2", "--dav", "3:3", "--advertise", "a:1", "--advertise", "b:2"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := cli.ChildArgs("", "", "", "", nil); !slices.Equal(got, []string{"daemon", "--foreground"}) {
		t.Errorf("no flags: %q", got)
	}
}
