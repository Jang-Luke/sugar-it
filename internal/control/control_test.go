package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
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

func TestParseTarget(t *testing.T) {
	ok := []struct {
		in   string
		want control.Target
	}{
		{"nas:media/a/b", control.Target{Peer: "nas", Share: "media", Path: "a/b"}},
		{"nas:media", control.Target{Peer: "nas", Share: "media"}},
		{"nas", control.Target{Peer: "nas"}},
		{"local:s/x y.txt", control.Target{Peer: "local", Share: "s", Path: "x y.txt"}},
		{"nas:media/", control.Target{Peer: "nas", Share: "media"}},
		{"nas:media/dir/", control.Target{Peer: "nas", Share: "media", Path: "dir/"}},
	}
	for _, c := range ok {
		got, err := control.ParseTarget(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseTarget(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{":x", "", "Bad Name:x", "nas:Bad", "nas:/x", "nas:media:x"} {
		if got, err := control.ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) = %+v, want an error", in, got)
		} else if vfs.CodeOf(err) != vfs.CodeInvalidRequest {
			t.Errorf("ParseTarget(%q) code %q", in, vfs.CodeOf(err))
		}
	}
}

// ---- fixtures ----

func freePort(t *testing.T) string {
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
	name string
	dir  string
	peer string // peer API address
	d    *daemon.Daemon
	c    *control.Client
}

func newHost(t *testing.T, name string) *host {
	t.Helper()
	h := &host{t: t, name: name, dir: t.TempDir(), peer: freePort(t)}
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
	h.start()
	return h
}

func (h *host) start() {
	h.t.Helper()
	d, err := daemon.Start(bg, daemon.Options{ConfigDir: h.dir})
	if err != nil {
		h.t.Fatal(err)
	}
	h.d = d
	h.t.Cleanup(h.stop)
	if h.c, err = control.Discover(h.dir); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) stop() {
	if h.d == nil {
		return
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	if err := h.d.Shutdown(ctx); err != nil {
		h.t.Errorf("shutdown %s: %v", h.name, err)
	}
	h.d = nil
}

func (h *host) id() string {
	st, err := h.c.Status(bg)
	if err != nil {
		h.t.Fatal(err)
	}
	return st.ID
}

func (h *host) share(name string, files map[string]string) string {
	h.t.Helper()
	dir := h.t.TempDir()
	for p, s := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.c.AddShare(bg, shares.Spec{Name: name, Path: dir}); err != nil {
		h.t.Fatal(err)
	}
	return dir
}

func pair(t *testing.T, inviter, joiner *host) {
	t.Helper()
	inv, err := inviter.c.Invite(bg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := joiner.c.JoinPeer(bg, inv); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := vfs.CodeOf(err); got != code || err == nil {
		t.Fatalf("error %v: code %q, want %q", err, got, code)
	}
}

// mesh3 starts hosts a, b, c, all paired with each other and online.
func mesh3(t *testing.T) (a, b, c *host) {
	t.Helper()
	a, b, c = newHost(t, "a"), newHost(t, "b"), newHost(t, "c")
	pair(t, a, b)
	pair(t, a, c)
	pair(t, b, c)
	for _, x := range []*host{a, b, c} {
		eventually(t, x.name+" sees two online peers", func() bool {
			st, err := x.c.Status(bg)
			if err != nil || len(st.Peers) != 2 {
				return false
			}
			return st.Peers[0].Online && st.Peers[1].Online
		})
	}
	return
}

func waitState(t *testing.T, c *control.Client, host, id string, want ...transfer.State) transfer.Job {
	t.Helper()
	var j transfer.Job
	eventually(t, "job state "+string(want[0]), func() bool {
		var err error
		if j, err = c.Job(bg, host, id); err != nil {
			return false
		}
		for _, w := range want {
			if j.State == w {
				return true
			}
		}
		return false
	})
	return j
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- auth ----

func rawGet(t *testing.T, addr, host, token string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+addr+"/v1/status", nil)
	if host != "" {
		req.Host = host
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestAuthRequired(t *testing.T) {
	h := newHost(t, "a")
	addr, token := h.d.ControlAddr(), h.c.Token()
	if token == "" {
		t.Fatal("client has no token")
	}
	for _, tok := range []string{"", "wrong-token", token + "x", token[:len(token)-1]} {
		code, body, _ := rawGet(t, addr, "", tok)
		if code != 401 {
			t.Errorf("token %q: status %d", tok, code)
		}
		if strings.Contains(body, token) || (tok != "" && strings.Contains(body, tok)) {
			t.Errorf("token %q: body leaks a token: %s", tok, body)
		}
	}
	if code, _, _ := rawGet(t, addr, "", token); code != 200 {
		t.Fatalf("valid token: status %d", code)
	}
	// A request with a wrong token must not reach any handler, mutating ones included.
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/invite", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated invite: %d", resp.StatusCode)
	}
}

func TestHostHeaderMustBeLoopback(t *testing.T) {
	h := newHost(t, "a")
	addr, token := h.d.ControlAddr(), h.c.Token()
	_, port, _ := net.SplitHostPort(addr)
	for _, host := range []string{"evil.example", "evil.example:" + port, "10.1.2.3:" + port, "127.0.0.1.evil.example"} {
		code, body, _ := rawGet(t, addr, host, token)
		if code != 403 {
			t.Errorf("Host %q: status %d, want 403", host, code)
		}
		if strings.Contains(body, token) {
			t.Errorf("Host %q: body leaks the token", host)
		}
	}
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "localhost", "[::1]:" + port} {
		if code, _, _ := rawGet(t, addr, host, token); code != 200 {
			t.Errorf("Host %q: status %d, want 200", host, code)
		}
	}
	_, _, hdr := rawGet(t, addr, "", token)
	if hdr.Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS must not be enabled")
	}
}

// ---- discovery ----

func TestDiscoverUnavailable(t *testing.T) {
	dir := t.TempDir()
	if _, err := control.Discover(dir); !errors.Is(err, control.ErrUnavailable) {
		t.Fatalf("no runtime.json: %v", err)
	}
	// A stale runtime.json: nothing listens behind it.
	dead := freePort(t)
	if _, err := config.ControlToken(dir); err != nil {
		t.Fatal(err)
	}
	rt, _ := json.Marshal(map[string]any{"ControlAddr": dead, "DAVAddr": dead, "PeerAddr": dead, "PID": 1})
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), rt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := control.Discover(dir)
	if !errors.Is(err, control.ErrUnavailable) {
		t.Fatalf("stale runtime.json: %v", err)
	}
	if tok, _ := config.ControlToken(dir); strings.Contains(err.Error(), tok) {
		t.Fatal("error message contains the token")
	}
	// Garbage is unavailable too, not a crash.
	os.WriteFile(filepath.Join(dir, "runtime.json"), []byte("{"), 0o600)
	if _, err := control.Discover(dir); !errors.Is(err, control.ErrUnavailable) {
		t.Fatalf("corrupt runtime.json: %v", err)
	}
}

func TestDiscoverAfterShutdownIsUnavailable(t *testing.T) {
	h := newHost(t, "a")
	if h.c.DAVAddr() == "" || h.c.DAVAddr() != h.d.DAVAddr() {
		t.Fatalf("DAVAddr %q vs %q", h.c.DAVAddr(), h.d.DAVAddr())
	}
	h.stop()
	if _, err := control.Discover(h.dir); !errors.Is(err, control.ErrUnavailable) {
		t.Fatalf("after shutdown: %v", err)
	}
	// The client of a dead daemon reports it as unavailable as well.
	if _, err := h.c.Status(bg); !errors.Is(err, control.ErrUnavailable) {
		t.Fatalf("Status on dead daemon: %v", err)
	}
}

// ---- basic operations ----

func TestStatusAndPeers(t *testing.T) {
	a, b, _ := mesh3(t)
	st, err := a.c.Status(bg)
	if err != nil || st.Name != "a" || st.ID == "" || st.Version == "" || st.InProcess {
		t.Fatalf("%+v %v", st, err)
	}
	if len(st.Peers) != 2 || st.Peers[0].Name != "b" {
		t.Fatalf("%+v", st.Peers)
	}
	if err := a.c.RemovePeer(bg, "b"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, a.c.RemovePeer(bg, "b"), vfs.CodePeerNotFound)
	wantCode(t, a.c.RemovePeer(bg, "local"), vfs.CodeInvalidRequest)
	if err := b.c.RenameSelf(bg, "bee"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, b.c.RenameSelf(bg, "Bad Name"), vfs.CodeInvalidRequest)
	if st, _ := b.c.Status(bg); st.Name != "bee" {
		t.Fatalf("%+v", st)
	}
}

func TestSharesAndFilesLocalAndRemote(t *testing.T) {
	a, b, _ := mesh3(t)
	dir := b.share("docs", map[string]string{"x.txt": "hi", "sub/y.txt": "yo"})
	b.share("ro", nil)

	infos, err := a.c.ListShares(bg, "b")
	if err != nil || len(infos) != 2 {
		t.Fatalf("%+v %v", infos, err)
	}
	if infos, _ := a.c.ListShares(bg, ""); len(infos) != 0 {
		t.Fatalf("a has no shares: %+v", infos)
	}
	_, err = a.c.ListShares(bg, "nosuch")
	wantCode(t, err, vfs.CodePeerNotFound)

	es, err := a.c.List(bg, control.Target{Peer: "b", Share: "docs"})
	if err != nil || len(es) != 2 {
		t.Fatalf("%+v %v", es, err)
	}
	if err := a.c.Mkdir(bg, control.Target{Peer: "b", Share: "docs", Path: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := a.c.Rename(bg, control.Target{Peer: "b", Share: "docs", Path: "x.txt"}, "z.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "z.txt")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, a.c.Rename(bg, control.Target{Peer: "b", Share: "docs", Path: "z.txt"}, "a/b"), vfs.CodeInvalidName)
	wantCode(t, a.c.Remove(bg, control.Target{Peer: "b", Share: "docs", Path: "sub"}, false), vfs.CodeNotEmpty)
	if err := a.c.Remove(bg, control.Target{Peer: "b", Share: "docs", Path: "sub"}, true); err != nil {
		t.Fatal(err)
	}
	_, err = a.c.List(bg, control.Target{Peer: "b", Share: "nosuch"})
	wantCode(t, err, vfs.CodeShareNotFound)
	_, err = a.c.List(bg, control.Target{Peer: "b"})
	wantCode(t, err, vfs.CodeInvalidRequest)

	// The same operations on this host's own shares.
	a.share("mine", map[string]string{"f": "1"})
	if err := a.c.Mkdir(bg, control.Target{Peer: "local", Share: "mine", Path: "d"}); err != nil {
		t.Fatal(err)
	}
	if es, _ := a.c.List(bg, control.Target{Peer: "local", Share: "mine"}); len(es) != 2 {
		t.Fatalf("%+v", es)
	}
	if err := a.c.RemoveShare(bg, "mine"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, a.c.RemoveShare(bg, "mine"), vfs.CodeShareNotFound)
}

// ---- transfers ----

func TestStartJobDelegatesToDestination(t *testing.T) {
	a, b, c := mesh3(t)
	b.share("s", map[string]string{"x": "payload"})
	cdir := c.share("s", nil)

	j, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/x", Dst: "c:s/"})
	if err != nil {
		t.Fatal(err)
	}
	if j.Host != c.id() || j.Origin != a.id() {
		t.Fatalf("host %q origin %q, want %q %q", j.Host, j.Origin, c.id(), a.id())
	}
	if j.Spec.Src.Peer != b.id() || j.Spec.Dst.Peer != c.id() {
		t.Fatalf("spec carries names instead of IDs: %+v", j.Spec)
	}
	if got, err := c.c.Jobs(bg); err != nil || len(got) != 1 || got[0].ID != j.ID {
		t.Fatalf("C's manager: %+v %v", got, err)
	}
	if got, _ := b.c.Jobs(bg); len(got) != 0 {
		t.Fatalf("B ran something: %+v", got)
	}
	done := waitState(t, a.c, j.Host, j.ID, transfer.StateDone)
	if done.DoneFiles != 1 {
		t.Fatalf("%+v", done)
	}
	if got := readFile(t, filepath.Join(cdir, "x")); got != "payload" {
		t.Fatalf("got %q", got)
	}
}

func TestStartJobDestLocal(t *testing.T) {
	a, b, _ := mesh3(t)
	b.share("s", map[string]string{"x": "local-dest"})
	adir := a.share("s", nil)
	j, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/x", Dst: "local:s/"})
	if err != nil {
		t.Fatal(err)
	}
	if j.Host != a.id() || j.Origin != a.id() {
		t.Fatalf("%q %q", j.Host, j.Origin)
	}
	waitState(t, a.c, j.Host, j.ID, transfer.StateDone)
	if got := readFile(t, filepath.Join(adir, "x")); got != "local-dest" {
		t.Fatalf("got %q", got)
	}
}

func TestStartJobRejectsBadRequests(t *testing.T) {
	a, b, c := mesh3(t)
	b.share("s", map[string]string{"x": "1"})
	c.share("s", nil)
	for _, r := range []control.TransferRequest{
		{Op: "copy", Src: "", Dst: "c:s/"},
		{Op: "copy", Src: "b:s/x", Dst: "c"},
		{Op: "copy", Src: "b", Dst: "c:s/"},
		{Op: "link", Src: "b:s/x", Dst: "c:s/"},
		{Op: "copy", Src: "b:s/x", Dst: "c:s/", OnConflict: "nonsense"},
	} {
		_, err := a.c.StartJob(bg, r)
		wantCode(t, err, vfs.CodeInvalidRequest)
	}
	_, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "nosuch:s/x", Dst: "c:s/"})
	wantCode(t, err, vfs.CodePeerNotFound)
	_, err = a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/x", Dst: "nosuch:s/"})
	wantCode(t, err, vfs.CodePeerNotFound)
	_, err = a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/x", Dst: "c:nosuch/"})
	wantCode(t, err, vfs.CodeShareNotFound)
}

func TestJobsAggregates(t *testing.T) {
	a, b, c := mesh3(t)
	a.share("s", map[string]string{"x": "from-a"})
	c.share("s", map[string]string{"y": "from-c"})
	a.share("t", nil)
	c.share("t", nil)
	j1, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "local:s/x", Dst: "c:t/"}) // runs on C
	if err != nil {
		t.Fatal(err)
	}
	j2, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "c:s/y", Dst: "local:t/"}) // runs on A
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a.c, j1.Host, j1.ID, transfer.StateDone)
	waitState(t, a.c, j2.Host, j2.ID, transfer.StateDone)
	// A job on C that A did not request must not show up in A's list.
	if _, err := c.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "local:s/y", Dst: "local:t/other"}); err != nil {
		t.Fatal(err)
	}

	check := func(what string) {
		t.Helper()
		got, err := a.c.Jobs(bg)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		ids := map[string]string{}
		for _, j := range got {
			ids[j.ID] = j.Host
		}
		if len(got) != 2 || ids[j1.ID] != c.id() || ids[j2.ID] != a.id() {
			t.Fatalf("%s: got %+v", what, got)
		}
	}
	check("all online")
	b.stop() // an offline peer must not hide the rest
	check("b offline")
}

func TestDstOfflineFailsFast(t *testing.T) {
	a, b, c := mesh3(t)
	b.share("s", map[string]string{"x": "1"})
	c.share("s", nil)
	c.stop()
	start := time.Now()
	_, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/x", Dst: "c:s/"})
	wantCode(t, err, vfs.CodePeerOffline)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	if got, _ := a.c.Jobs(bg); len(got) != 0 {
		t.Fatalf("a job was created on A: %+v", got)
	}
	c.start()
	if got, _ := c.c.Jobs(bg); len(got) != 0 {
		t.Fatalf("a job was created on C: %+v", got)
	}
}

func TestSourceUnreachableFromDest(t *testing.T) {
	a, b, c := mesh3(t)
	b.share("s", map[string]string{"x": "1"})
	cdir := c.share("s", nil)
	b.stop() // C cannot reach B; A is not asked to carry the data
	j, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/x", Dst: "c:s/"})
	if err != nil {
		t.Fatal(err)
	}
	got := waitState(t, a.c, j.Host, j.ID, transfer.StateWaitingPeer)
	if got.Host != c.id() {
		t.Fatalf("%+v", got)
	}
	var seen bool
	jobs, _ := a.c.Jobs(bg)
	for _, x := range jobs {
		seen = seen || (x.ID == j.ID && x.State == transfer.StateWaitingPeer)
	}
	if !seen {
		t.Fatalf("A's Jobs does not show the waiting job: %+v", jobs)
	}
	// When B returns, the job finishes by itself.
	b.start()
	waitState(t, a.c, j.Host, j.ID, transfer.StateDone)
	if got := readFile(t, filepath.Join(cdir, "x")); got != "1" {
		t.Fatalf("got %q", got)
	}
}

func TestRemoteJobCancelAndRetry(t *testing.T) {
	a, b, c := mesh3(t)
	b.share("s", map[string]string{"x": "1"})
	c.share("s", nil)
	b.stop()
	j, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "b:s/x", Dst: "c:s/"})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a.c, j.Host, j.ID, transfer.StateWaitingPeer)
	if err := a.c.CancelJob(bg, j.Host, j.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, a.c, j.Host, j.ID, transfer.StateCanceled)
	if err := a.c.RetryJob(bg, j.Host, j.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, a.c, j.Host, j.ID, transfer.StateWaitingPeer, transfer.StateQueued, transfer.StateRunning)
	wantCode(t, a.c.CancelJob(bg, j.Host, "nosuch"), vfs.CodeNotFound)
	_, err = a.c.Job(bg, j.Host, "nosuch")
	wantCode(t, err, vfs.CodeNotFound)
	_, err = a.c.Job(bg, "notamember", j.ID)
	wantCode(t, err, vfs.CodePeerNotFound)
	// Local jobs are routed locally.
	a.share("s", map[string]string{"x": "1"})
	a.share("t", nil)
	lj, err := a.c.StartJob(bg, control.TransferRequest{Op: "copy", Src: "local:s/x", Dst: "local:t/"})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a.c, lj.Host, lj.ID, transfer.StateDone)
	wantCode(t, a.c.CancelJob(bg, lj.Host, lj.ID), vfs.CodeInvalidRequest) // already finished
}
