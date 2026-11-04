package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sugarintegration/internal/cli"
	"sugarintegration/internal/config"
	"sugarintegration/internal/daemon"
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

type host struct {
	t    *testing.T
	dir  string
	peer string
	d    *daemon.Daemon
}

// newConfig prepares a config dir for a host on loopback ports; no daemon.
func newConfig(t *testing.T, name string) *host {
	t.Helper()
	h := &host{t: t, dir: t.TempDir(), peer: freeAddr(t)}
	cfg, err := config.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Name, c.ListenAddr, c.AdvertiseAddrs = name, h.peer, []string{h.peer}
		c.ControlAddr, c.DAVAddr = "127.0.0.1:0", "127.0.0.1:0"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func newHost(t *testing.T, name string) *host {
	t.Helper()
	h := newConfig(t, name)
	// The peer address has to be known before binding, so another process can
	// take it first; choose a new one when that happens.
	d, err := daemon.Start(bg, daemon.Options{ConfigDir: h.dir})
	for attempt := 0; err != nil && strings.Contains(err.Error(), "address already in use") && attempt < 5; attempt++ {
		h.peer = freeAddr(t)
		cfg, cerr := config.Open(h.dir)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if cerr := cfg.Update(func(c *config.Config) error {
			c.ListenAddr, c.AdvertiseAddrs = h.peer, []string{h.peer}
			return nil
		}); cerr != nil {
			t.Fatal(cerr)
		}
		d, err = daemon.Start(bg, daemon.Options{ConfigDir: h.dir})
	}
	if err != nil {
		t.Fatal(err)
	}
	h.d = d
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		d.Shutdown(ctx)
	})
	return h
}

// run executes the CLI against h and returns exit code, stdout and stderr.
func (h *host) run(args ...string) (int, string, string) {
	h.t.Helper()
	var out, errb bytes.Buffer
	code := cli.Execute(append([]string{"--config-dir", h.dir}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func (h *host) ok(args ...string) string {
	h.t.Helper()
	code, out, errs := h.run(args...)
	if code != 0 {
		h.t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
	}
	return out
}

// pair returns hosts a and b, paired, each with share s (a) and t (b).
func pair(t *testing.T) (a, b *host, adir, bdir string) {
	t.Helper()
	a, b = newHost(t, "a"), newHost(t, "b")
	adir, bdir = t.TempDir(), t.TempDir()
	inv := strings.TrimSpace(a.ok("invite"))
	b.ok("peer", "add", inv)
	a.ok("share", "add", "s", adir)
	b.ok("share", "add", "t", bdir)
	return
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPairAndShareFlow(t *testing.T) {
	a, b, adir, _ := pair(t)
	write(t, filepath.Join(adir, "hello.txt"), "hi")

	var peers []struct{ Name string }
	if err := json.Unmarshal([]byte(b.ok("peer", "list", "--json")), &peers); err != nil || len(peers) != 1 || peers[0].Name != "a" {
		t.Fatalf("peer list: %v %+v", err, peers)
	}
	if out := b.ok("ls", "a:s"); !strings.Contains(out, "hello.txt") {
		t.Fatalf("ls a:s: %q", out)
	}
	if out := b.ok("ls", "a"); !strings.Contains(out, "s") {
		t.Fatalf("ls a: %q", out)
	}
	if out := a.ok("share", "list"); !strings.Contains(out, "s") {
		t.Fatalf("share list: %q", out)
	}
	a.ok("share", "remove", "s")
	if out := a.ok("share", "list", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("share not removed: %q", out)
	}
}

func TestPeerRenameAndRemove(t *testing.T) {
	_, b, _, _ := pair(t)
	b.ok("peer", "rename", "bee")
	var st struct{ Name string }
	if err := json.Unmarshal([]byte(b.ok("status", "--json")), &st); err != nil || st.Name != "bee" {
		t.Fatalf("status after rename: %v %+v", err, st)
	}
	b.ok("peer", "remove", "a")
	var peers []struct{ Name string }
	if err := json.Unmarshal([]byte(b.ok("peer", "list", "--json")), &peers); err != nil || len(peers) != 0 {
		t.Fatalf("peers after remove: %v %+v", err, peers)
	}
}

func TestCpWaitsAndExitCodes(t *testing.T) {
	_, b, adir, bdir := pair(t)
	write(t, filepath.Join(adir, "x.txt"), "payload")
	code, out, errs := b.run("cp", "a:s/x.txt", "local:t/x.txt")
	if code != 0 {
		t.Fatalf("cp exit %d\n%s\n%s", code, out, errs)
	}
	if got, _ := os.ReadFile(filepath.Join(bdir, "x.txt")); string(got) != "payload" {
		t.Fatalf("copied content %q", got)
	}
	// Again: the destination exists and the policy is fail.
	code, _, errs = b.run("cp", "a:s/x.txt", "local:t/x.txt")
	if code != 1 || !strings.Contains(errs, "x.txt") {
		t.Fatalf("conflicting cp: exit %d stderr %q", code, errs)
	}
	if code, _, errs = b.run("cp", "--on-conflict", "overwrite", "a:s/x.txt", "local:t/x.txt"); code != 0 {
		t.Fatalf("overwrite: exit %d %s", code, errs)
	}
}

func TestMvDetachReturnsJobID(t *testing.T) {
	_, b, adir, bdir := pair(t)
	write(t, filepath.Join(adir, "m.txt"), "move me")
	out := strings.TrimSpace(b.ok("mv", "--detach", "a:s/m.txt", "local:t/m.txt"))
	if out == "" || strings.ContainsAny(out, " \n") {
		t.Fatalf("detach output %q, want only a job id", out)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(adir, "m.txt")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("move never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got, _ := os.ReadFile(filepath.Join(bdir, "m.txt")); string(got) != "move me" {
		t.Fatalf("moved content %q", got)
	}
	if out := b.ok("jobs", "show", out); !strings.Contains(out, "done") {
		t.Fatalf("jobs show: %q", out)
	}
}

func TestUsageErrorExit2(t *testing.T) {
	_, b, _, _ := pair(t)
	for _, args := range [][]string{
		{"cp", "a:s/x"},
		{"cp", "--on-conflict", "bogus", "a:s/x", "local:t/x"},
		{"cp", "Bad Name:s/x", "local:t/x"},
		{"ls"},
		{"ls", "a:Bad"},
		{"rm"},
		{"nosuchcommand"},
		{"ls", "--nosuchflag", "a"},
		{"share", "add", "s"},
	} {
		if code, _, errs := b.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (stderr %q)", args, code, errs)
		}
	}
}

func TestDaemonNotRunningHint(t *testing.T) {
	h := newConfig(t, "n")
	code, _, errs := h.run("status")
	if code != 1 || !strings.Contains(errs, "sugar-it daemon") {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
}

func TestJSONOutputParses(t *testing.T) {
	_, b, adir, _ := pair(t)
	write(t, filepath.Join(adir, "j.txt"), "j")
	b.ok("cp", "a:s/j.txt", "local:t/j.txt")
	for _, args := range [][]string{
		{"status"}, {"peer", "list"}, {"share", "list"}, {"ls", "a:s"}, {"ls", "local:t"}, {"jobs", "list"},
	} {
		out := b.ok(append(args, "--json")...)
		if !json.Valid([]byte(out)) {
			t.Errorf("%v --json: invalid JSON %q", args, out)
		}
	}
	// An empty listing is [], not null.
	if out := b.ok("jobs", "list", "--json"); strings.TrimSpace(out) == "null" {
		t.Errorf("jobs list printed null")
	}
}

func TestStatusNeverPrintsToken(t *testing.T) {
	h := newHost(t, "tk")
	tok, err := config.ControlToken(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status"}, {"status", "--json"}, {"invite"}, {"peer", "list"}} {
		_, out, errs := h.run(args...)
		if strings.Contains(out+errs, tok) {
			t.Fatalf("%v leaked the control token", args)
		}
	}
}

func TestInvitePrintsOnlyInvite(t *testing.T) {
	a := newHost(t, "a")
	out := a.ok("invite", "--ttl", "5m")
	if !strings.HasPrefix(out, "sugar-it1:") || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("invite output %q", out)
	}
}

func TestRmRequiresRForDir(t *testing.T) {
	_, b, _, bdir := pair(t)
	write(t, filepath.Join(bdir, "d", "f.txt"), "f")
	code, _, errs := b.run("rm", "local:t/d")
	if code != 1 || !strings.Contains(errs, "not_empty") {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	b.ok("rm", "-r", "local:t/d")
	if _, err := os.Stat(filepath.Join(bdir, "d")); !os.IsNotExist(err) {
		t.Fatalf("dir still there: %v", err)
	}
}

func TestMkdirAndRename(t *testing.T) {
	_, b, _, bdir := pair(t)
	b.ok("mkdir", "local:t/new")
	b.ok("rename", "local:t/new", "renamed")
	if st, err := os.Stat(filepath.Join(bdir, "renamed")); err != nil || !st.IsDir() {
		t.Fatalf("renamed dir: %v", err)
	}
}

func TestSharePathRelativeResolved(t *testing.T) {
	h := newHost(t, "r")
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	h.ok("share", "add", "s", "./x")
	cfg, err := config.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Get().Shares
	if len(got) != 1 || !filepath.IsAbs(got[0].Path) || filepath.Base(got[0].Path) != "x" {
		t.Fatalf("stored shares %+v", got)
	}
}

func TestSpacesAndKoreanPaths(t *testing.T) {
	h := newHost(t, "k")
	dir := t.TempDir()
	h.ok("share", "add", "s", dir)
	write(t, filepath.Join(dir, "내 문서", "a b.txt"), "한글")
	if out := h.ok("ls", "local:s/내 문서"); !strings.Contains(out, "a b.txt") {
		t.Fatalf("ls: %q", out)
	}
	h.ok("cp", "local:s/내 문서/a b.txt", "local:s/내 문서/c d.txt")
	if got, _ := os.ReadFile(filepath.Join(dir, "내 문서", "c d.txt")); string(got) != "한글" {
		t.Fatalf("copy content %q", got)
	}
	h.ok("rm", "local:s/내 문서/c d.txt")
	if _, err := os.Stat(filepath.Join(dir, "내 문서", "c d.txt")); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
}

func TestJobsCancelRetryNeedKnownID(t *testing.T) {
	h := newHost(t, "j")
	if code, _, errs := h.run("jobs", "cancel", "nosuchid"); code != 1 || !strings.Contains(errs, "nosuchid") {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
}

func TestVersionFlag(t *testing.T) {
	old := cli.Version
	cli.Version = "v9.8.7-test"
	t.Cleanup(func() { cli.Version = old })
	var out, errb bytes.Buffer
	if code := cli.Execute([]string{"--version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "v9.8.7-test") {
		t.Fatalf("--version output %q lacks the injected version", out.String())
	}
}

func TestLsMarksSymlinks(t *testing.T) {
	h := newHost(t, "a")
	dir := t.TempDir()
	h.ok("share", "add", "s", dir)
	write(t, filepath.Join(dir, "file.txt"), "12345")
	if err := os.Symlink("file.txt", filepath.Join(dir, "link")); err != nil {
		t.Skip(err)
	}
	out := h.ok("ls", "local:s")
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "link") {
			line = l
		}
	}
	if !strings.HasSuffix(line, "link@") || !strings.HasPrefix(line, "-") {
		t.Errorf("symlink line %q in:\n%s", line, out)
	}
	if !strings.Contains(out, "5 B") {
		t.Errorf("file size missing:\n%s", out)
	}
}
