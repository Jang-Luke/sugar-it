package daemon_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
	"sugarintegration/internal/daemon"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

var bg = context.Background()

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// setup writes a config for a daemon on fixed loopback ports.
func setup(t *testing.T, name string) (dir string, o daemon.Options) {
	t.Helper()
	dir = t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	peer := freeAddr(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Name, c.ListenAddr, c.AdvertiseAddrs = name, peer, []string{peer}
		c.ControlAddr, c.DAVAddr = freeAddr(t), freeAddr(t)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dir, daemon.Options{ConfigDir: dir}
}

func start(t *testing.T, o daemon.Options) *daemon.Daemon {
	t.Helper()
	d, err := daemon.Start(bg, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	return d
}

func stop(t *testing.T, d *daemon.Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if err := d.Wait(); err != nil {
		t.Errorf("wait: %v", err)
	}
}

func dialable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartStop(t *testing.T) {
	dir, o := setup(t, "alpha")
	cfg := mustConfig(t, dir)
	d, err := daemon.Start(bg, o)
	if err != nil {
		t.Fatal(err)
	}
	if d.PeerAddr() != cfg.ListenAddr || d.ControlAddr() != cfg.ControlAddr || d.DAVAddr() != cfg.DAVAddr {
		t.Fatalf("addresses %q %q %q, config %+v", d.PeerAddr(), d.ControlAddr(), d.DAVAddr(), cfg)
	}
	for _, a := range []string{d.PeerAddr(), d.ControlAddr(), d.DAVAddr()} {
		if !dialable(a) {
			t.Fatalf("%s is not listening", a)
		}
	}
	if d.Node() == nil {
		t.Fatal("no node")
	}
	// runtime.json holds the bound addresses and never the token.
	raw, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := config.ControlToken(dir)
	if strings.Contains(string(raw), tok) {
		t.Fatal("runtime.json contains the control token")
	}
	var rt control.Runtime
	if err := json.Unmarshal(raw, &rt); err != nil || rt.ControlAddr != d.ControlAddr() || rt.DAVAddr != d.DAVAddr() {
		t.Fatalf("%s %v", raw, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "runtime.json")); fi.Mode().Perm() != 0o600 && !isWindows() {
		t.Fatalf("mode %v", fi.Mode())
	}
	// DAV answers 503 for now.
	resp, err := http.Get("http://" + d.DAVAddr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("DAV status %d", resp.StatusCode)
	}

	stop(t, d)
	for _, a := range []string{d.PeerAddr(), d.ControlAddr(), d.DAVAddr()} {
		if dialable(a) {
			t.Fatalf("%s still listens after Shutdown", a)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime.json")); !os.IsNotExist(err) {
		t.Fatalf("runtime.json after shutdown: %v", err)
	}
	// The ports can be used again, by a fresh start.
	d2 := start(t, o)
	if d2.PeerAddr() != d.PeerAddr() {
		t.Fatalf("%q vs %q", d2.PeerAddr(), d.PeerAddr())
	}
	// Shutdown twice is fine.
	stop(t, d2)
	stop(t, d2)
}

func isWindows() bool { return os.PathSeparator == '\\' }

func mustConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	s, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s.Get()
}

func TestOptionsOverrideConfigAndPortZero(t *testing.T) {
	dir, _ := setup(t, "alpha")
	d := start(t, daemon.Options{ConfigDir: dir, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0", DAVAddr: "127.0.0.1:0", InProcess: true})
	for _, a := range []string{d.PeerAddr(), d.ControlAddr(), d.DAVAddr()} {
		if _, p, _ := net.SplitHostPort(a); p == "0" || !dialable(a) {
			t.Fatalf("%q is not a bound address", a)
		}
	}
	c, err := control.Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(bg)
	if err != nil || !st.InProcess {
		t.Fatalf("%+v %v", st, err)
	}
	if c.DAVAddr() != d.DAVAddr() {
		t.Fatalf("%q vs %q", c.DAVAddr(), d.DAVAddr())
	}
}

func TestPortZeroAdvertisesBoundPort(t *testing.T) {
	// No advertise_addrs: the node must use the port that was actually bound.
	dir := t.TempDir()
	cfg, _ := config.Open(dir)
	cfg.Update(func(c *config.Config) error {
		c.Name, c.ListenAddr, c.ControlAddr, c.DAVAddr = "alpha", "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
		return nil
	})
	d := start(t, daemon.Options{ConfigDir: dir})
	_, port, _ := net.SplitHostPort(d.PeerAddr())
	// With only loopback interfaces nothing is advertised, but a port 0 must
	// never be: the member record may not mention ":0".
	for _, m := range mustConfig(t, dir).Members {
		for _, a := range m.Addrs {
			if strings.HasSuffix(a, ":0") {
				t.Fatalf("advertised %q (bound port %s)", a, port)
			}
		}
	}
}

func TestPortInUseClearError(t *testing.T) {
	for _, which := range []string{"peer", "control", "dav"} {
		t.Run(which, func(t *testing.T) {
			dir, o := setup(t, "alpha")
			busy, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer busy.Close()
			addr := busy.Addr().String()
			switch which {
			case "peer":
				o.ListenAddr = addr
			case "control":
				o.ControlAddr = addr
			case "dav":
				o.DAVAddr = addr
			}
			d, err := daemon.Start(bg, o)
			if err == nil {
				stop(t, d)
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), addr) || !strings.Contains(err.Error(), which) {
				t.Fatalf("error %q does not name the listener and %s", err, addr)
			}
			if tok, _ := config.ControlToken(dir); strings.Contains(err.Error(), tok) {
				t.Fatal("error contains the token")
			}
			if _, err := os.Stat(filepath.Join(dir, "runtime.json")); !os.IsNotExist(err) {
				t.Fatalf("runtime.json left behind: %v", err)
			}
			// Nothing stays bound after the failure.
			busy.Close()
			d = start(t, o)
			_ = d
		})
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	dir, o := setup(t, "alpha")
	first := start(t, o)
	before, _ := os.ReadFile(filepath.Join(dir, "runtime.json"))

	// Same addresses, and addresses that differ: both are refused by the lock.
	for name, o2 := range map[string]daemon.Options{
		"same":      o,
		"different": {ConfigDir: dir, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0", DAVAddr: "127.0.0.1:0"},
	} {
		second, err := daemon.Start(bg, o2)
		if err == nil {
			stop(t, second)
			t.Fatalf("%s: second daemon started", name)
		}
		if !strings.Contains(err.Error(), dir) {
			t.Fatalf("%s: error %q does not name the config directory", name, err)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if string(before) != string(after) {
		t.Fatalf("runtime.json changed: %s -> %s", before, after)
	}
	c, err := control.Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(bg); err != nil {
		t.Fatalf("first daemon is gone: %v", err)
	}
	if !dialable(first.PeerAddr()) || !dialable(first.DAVAddr()) {
		t.Fatal("first daemon lost a listener")
	}
}

func TestSecondDaemonLeavesJobsAndPartsAlone(t *testing.T) {
	_, b, adir, bdir, ao, _ := twoDaemons(t)
	addShare(t, bdir, "s", map[string]string{"x": "still mine"})
	dst := addShare(t, adir, "d", nil)
	ac, _ := control.Discover(adir)
	stop(t, b)
	j, err := ac.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/x", Dst: "local:d/"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "waiting_peer", func() bool {
		j, err := ac.Job(bg, j.Host, j.ID)
		return err == nil && j.State == transfer.StateWaitingPeer
	})
	// A stale part the second daemon's GC would delete if it ran.
	partDir := filepath.Join(dst, vfs.PartDir)
	os.MkdirAll(partDir, 0o700)
	stale := filepath.Join(partDir, "orphan.part")
	os.WriteFile(stale, []byte("x"), 0o600)
	old := time.Now().Add(-9 * 24 * time.Hour)
	os.Chtimes(stale, old, old)

	o2 := daemon.Options{ConfigDir: adir, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0", DAVAddr: "127.0.0.1:0"}
	if d, err := daemon.Start(bg, o2); err == nil {
		stop(t, d)
		t.Fatal("second daemon started")
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the refused daemon ran part cleanup: %v", err)
	}
	if got, err := ac.Job(bg, j.Host, j.ID); err != nil || got.State != transfer.StateWaitingPeer {
		t.Fatalf("first daemon's job: %+v %v", got, err)
	}
	// The first daemon still finishes its job when B returns.
	start(t, daemon.Options{ConfigDir: bdir})
	eventually(t, "done", func() bool {
		got, err := ac.Job(bg, j.Host, j.ID)
		return err == nil && got.State == transfer.StateDone
	})
	_ = ao
}

func TestLockReleasedAfterShutdown(t *testing.T) {
	_, o := setup(t, "alpha")
	d := start(t, o)
	stop(t, d)
	d2, err := daemon.Start(bg, daemon.Options{ConfigDir: o.ConfigDir, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0", DAVAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("restart after Shutdown: %v", err)
	}
	stop(t, d2)
}

func TestLockFailedStartReleasesIt(t *testing.T) {
	_, o := setup(t, "alpha")
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	bad := o
	bad.DAVAddr = busy.Addr().String()
	if _, err := daemon.Start(bg, bad); err == nil {
		t.Fatal("expected a bind error")
	}
	start(t, o) // the lock did not leak
}

// TestHelperDaemon is not a test: the child process of
// TestLockReleasedAfterKill runs a daemon in it.
func TestHelperDaemon(t *testing.T) {
	dir := os.Getenv("SUGAR_IT_HELPER_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	if _, err := daemon.Start(bg, daemon.Options{ConfigDir: dir}); err != nil {
		fmt.Println("ERR", err)
		os.Exit(2)
	}
	fmt.Println("READY")
	select {}
}

func TestLockReleasedAfterKill(t *testing.T) {
	dir, o := setup(t, "alpha")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDaemon$")
	cmd.Env = append(os.Environ(), "SUGAR_IT_HELPER_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	ready := false
	for sc.Scan() {
		if sc.Text() == "READY" {
			ready = true
			break
		}
		if strings.HasPrefix(sc.Text(), "ERR") {
			t.Fatal(sc.Text())
		}
	}
	if !ready {
		t.Fatal("helper daemon did not start")
	}
	if d, err := daemon.Start(bg, daemon.Options{ConfigDir: dir, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0", DAVAddr: "127.0.0.1:0"}); err == nil {
		stop(t, d)
		t.Fatal("started next to a live daemon in another process")
	}
	cmd.Process.Kill()
	cmd.Wait()
	// The dead process left runtime.json behind; the lock is gone anyway.
	d := start(t, o)
	if _, err := control.Discover(dir); err != nil {
		t.Fatal(err)
	}
	_ = d
}

func TestContextCancelStopsDaemon(t *testing.T) {
	_, o := setup(t, "alpha")
	ctx, cancel := context.WithCancel(bg)
	d, err := daemon.Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- d.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after the context was canceled")
	}
	if dialable(d.ControlAddr()) {
		t.Fatal("still listening")
	}
}

func TestStartCollectsOnlyStaleInactiveParts(t *testing.T) {
	dir, o := setup(t, "alpha")
	good := t.TempDir()
	partDir := filepath.Join(good, vfs.PartDir)
	os.MkdirAll(partDir, 0o700)
	old := time.Now().Add(-8 * 24 * time.Hour)
	for name, mt := range map[string]time.Time{"stale.part": old, "stale.etag": old, "fresh.part": time.Now()} {
		p := filepath.Join(partDir, name)
		os.WriteFile(p, []byte("x"), 0o600)
		os.Chtimes(p, mt, mt)
	}
	cfg, _ := config.Open(dir)
	cfg.Update(func(c *config.Config) error {
		// A share whose directory is gone must not stop the daemon.
		c.Shares = []shares.Spec{{Name: "gone", Path: filepath.Join(dir, "no-such-dir")}, {Name: "good", Path: good}}
		return nil
	})
	d := start(t, o)
	_ = d
	for name, want := range map[string]bool{"stale.part": false, "stale.etag": false, "fresh.part": true} {
		_, err := os.Stat(filepath.Join(partDir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
}

// ---- jobs survive a restart ----

func twoDaemons(t *testing.T) (a, b *daemon.Daemon, adir, bdir string, ao, bo daemon.Options) {
	t.Helper()
	adir, ao = setup(t, "a")
	bdir, bo = setup(t, "b")
	a, b = start(t, ao), start(t, bo)
	ac, err := control.Discover(adir)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := control.Discover(bdir)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := ac.Invite(bg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.JoinPeer(bg, inv); err != nil {
		t.Fatal(err)
	}
	return
}

func addShare(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	c, err := control.Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for p, s := range files {
		if err := os.WriteFile(filepath.Join(src, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.AddShare(bg, shares.Spec{Name: name, Path: src}); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestGracefulShutdownKeepsJobs(t *testing.T) {
	a, b, adir, bdir, ao, _ := twoDaemons(t)
	addShare(t, bdir, "s", map[string]string{"x": "resume me"})
	dst := addShare(t, adir, "d", nil)
	ac, _ := control.Discover(adir)
	bc, _ := control.Discover(bdir)

	// B goes away, so the job on A waits for it.
	stop(t, b)
	j, err := ac.StartJob(bg, control.TransferRequest{Op: "move", Src: "b:s/x", Dst: "local:d/"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "waiting_peer", func() bool {
		j, err := ac.Job(bg, j.Host, j.ID)
		return err == nil && j.State == transfer.StateWaitingPeer
	})

	// Shut A down mid-job and start it again: the job is not failed, it resumes.
	stop(t, a)
	a2 := start(t, ao)
	_ = a2
	ac, err = control.Discover(adir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ac.Job(bg, j.Host, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == transfer.StateFailed || got.State == transfer.StateCanceled || got.State == transfer.StatePartial {
		t.Fatalf("job is %s after restart", got.State)
	}
	// B returns: the job completes and the move removes the source.
	bo2 := daemon.Options{ConfigDir: bdir}
	b2 := start(t, bo2)
	_ = b2
	bc, _ = control.Discover(bdir)
	_ = bc
	eventually(t, "done", func() bool {
		j, err := ac.Job(bg, j.Host, j.ID)
		return err == nil && j.State == transfer.StateDone
	})
	if got, err := os.ReadFile(filepath.Join(dst, "x")); err != nil || string(got) != "resume me" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestShutdownDuringTransferResumes(t *testing.T) {
	a, _, adir, bdir, ao, _ := twoDaemons(t)
	const n = 120
	files := map[string]string{}
	for i := range n {
		files[fmt.Sprintf("f%03d", i)] = strings.Repeat("data", 1000+i)
	}
	srcDir := addShare(t, bdir, "s", files)
	dst := addShare(t, adir, "d", nil)
	ac, _ := control.Discover(adir)

	j, err := ac.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/", Dst: "local:d/out/"})
	if err != nil {
		t.Fatal(err)
	}
	var at transfer.Job
	eventually(t, "some progress", func() bool {
		at, _ = ac.Job(bg, j.Host, j.ID)
		return at.DoneFiles > 0 || at.State == transfer.StateDone
	})
	stop(t, a)
	if at.State == transfer.StateDone {
		t.Skip("the job finished before the shutdown; nothing to resume")
	}
	start(t, ao)
	ac, err = control.Discover(adir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ac.Job(bg, j.Host, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == transfer.StateFailed || got.State == transfer.StateCanceled {
		t.Fatalf("the job is %s after a restart (%+v)", got.State, got.Error)
	}
	eventually(t, "done after restart", func() bool {
		got, err = ac.Job(bg, j.Host, j.ID)
		return err == nil && got.State == transfer.StateDone
	})
	for name, want := range files {
		b, err := os.ReadFile(filepath.Join(dst, "out", name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: err %v, len %d want %d", name, err, len(b), len(want))
		}
		if _, err := os.Stat(filepath.Join(srcDir, name)); err != nil {
			t.Fatalf("a copy removed its source: %v", err)
		}
	}
}
