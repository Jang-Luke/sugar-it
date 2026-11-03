// Package e2e runs the real sugar-it binary as three daemons on loopback and
// drives them through the CLI and WebDAV, the way a user would.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

var binary string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sugar-it-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	binary = filepath.Join(dir, "sugar-it")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "sugarintegration/cmd/sugar-it")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building sugar-it: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

// node is one sugar-it daemon process with its own config directory.
type node struct {
	t                  *testing.T
	name               string
	dir, share         string
	peer, ctl, dav     string
	cmd                *exec.Cmd
	mu                 sync.Mutex
	stderr             bytes.Buffer
	exited             chan struct{}
	started, ranBefore bool
}

func (n *node) Write(p []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stderr.Write(p)
}

func (n *node) log() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stderr.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// newNode prepares a node and starts its daemon. Free ports are found by
// listening and closing, which races with other processes; a bind failure
// restarts the daemon on new ports.
func newNode(t *testing.T, name string) *node {
	t.Helper()
	n := &node{t: t, name: name, dir: t.TempDir(), share: t.TempDir()}
	t.Cleanup(func() {
		n.kill()
		if t.Failed() {
			t.Logf("---- daemon %s stderr ----\n%s", n.name, n.log())
		}
	})
	for attempt := 0; ; attempt++ {
		n.peer, n.ctl, n.dav = freeAddr(t), freeAddr(t), freeAddr(t)
		err := n.start()
		if err == nil {
			break
		}
		if attempt >= 5 || !strings.Contains(n.log(), "address already in use") {
			t.Fatalf("daemon %s: %v\n%s", name, err, n.log())
		}
	}
	n.mustCLI("peer", "rename", name)
	n.mustCLI("share", "add", "s", n.share)
	return n
}

// start launches the daemon on the node's addresses and waits until it answers.
func (n *node) start() error {
	n.mu.Lock()
	n.stderr.Reset()
	n.mu.Unlock()
	n.cmd = exec.Command(binary, "--config-dir", n.dir, "daemon",
		"--listen", n.peer, "--control", n.ctl, "--dav", n.dav, "--advertise", n.peer)
	n.cmd.Stderr = n
	if err := n.cmd.Start(); err != nil {
		return err
	}
	n.exited = make(chan struct{})
	cmd, exited := n.cmd, n.exited
	go func() { cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case <-exited:
			return fmt.Errorf("daemon exited: %v", cmd.ProcessState)
		default:
		}
		if strings.Contains(n.log(), "daemon running") {
			return nil
		}
		if time.Now().After(deadline) {
			n.kill()
			return fmt.Errorf("daemon did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// restart starts the daemon again on the same addresses and config directory.
// The addresses are known to the other nodes, so a bind failure is retried
// rather than moved.
func (n *node) restart() {
	n.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := n.start()
		if err == nil {
			return
		}
		if !strings.Contains(n.log(), "address already in use") || time.Now().After(deadline) {
			n.t.Fatalf("restart %s: %v\n%s", n.name, err, n.log())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (n *node) kill() {
	if n.cmd == nil || n.cmd.Process == nil {
		return
	}
	n.cmd.Process.Kill()
	<-n.exited
}

// cliResult is the outcome of one CLI invocation.
type cliResult struct {
	out, err string
	code     int
}

func (n *node) cli(args ...string) cliResult {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"--config-dir", n.dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			n.t.Fatalf("%s: sugar-it %v: %v", n.name, args, err)
		}
		code = ee.ExitCode()
	}
	return cliResult{strings.TrimSpace(out.String()), strings.TrimSpace(errb.String()), code}
}

func (n *node) mustCLI(args ...string) string {
	n.t.Helper()
	r := n.cli(args...)
	if r.code != 0 {
		n.t.Fatalf("%s: sugar-it %v: exit %d\nstdout: %s\nstderr: %s", n.name, args, r.code, r.out, r.err)
	}
	return r.out
}

func (n *node) jobs() []transfer.Job {
	n.t.Helper()
	var js []transfer.Job
	if err := json.Unmarshal([]byte(n.mustCLI("jobs", "list", "--json")), &js); err != nil {
		n.t.Fatal(err)
	}
	return js
}

// waitJob polls jobs list on n until the job satisfies ok, with a deadline.
func (n *node) waitJob(id string, d time.Duration, what string, ok func(transfer.Job) bool) transfer.Job {
	n.t.Helper()
	deadline := time.Now().Add(d)
	var last transfer.Job
	for {
		for _, j := range n.jobs() {
			if j.ID == id {
				last = j
				if ok(j) {
					return j
				}
			}
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("%s: job %s never %s; last state %+v", n.name, id, what, last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func isDone(j transfer.Job) bool { return j.State == transfer.StateDone }

// mesh pairs a-b and b-c and waits until every node sees both others online.
func mesh(t *testing.T) (a, b, c *node) {
	t.Helper()
	a, b, c = newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	b.mustCLI("peer", "add", a.mustCLI("invite"))
	c.mustCLI("peer", "add", b.mustCLI("invite"))
	for _, n := range []*node{a, b, c} {
		n.waitPeers(2)
	}
	return
}

func (n *node) waitPeers(count int) {
	n.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var ps []struct {
			Name   string
			Online bool
		}
		if err := json.Unmarshal([]byte(n.mustCLI("peer", "list", "--json")), &ps); err != nil {
			n.t.Fatal(err)
		}
		online := 0
		for _, p := range ps {
			if p.Online {
				online++
			}
		}
		if online >= count {
			return
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("%s sees %d online peers, want %d: %+v", n.name, online, count, ps)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// writeRandom writes size pseudo-random bytes to path and returns their SHA-256.
func writeRandom(t *testing.T, path string, size int64, seed uint64) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	rng := rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8)})
	buf := make([]byte, 1<<20)
	for left := size; left > 0; {
		n := int64(len(buf))
		if left < n {
			n = left
		}
		rng.Read(buf[:n])
		h.Write(buf[:n])
		if _, err := f.Write(buf[:n]); err != nil {
			t.Fatal(err)
		}
		left -= n
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// snapshot lists every path under dir with its size, for "nothing changed" checks.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if strings.HasPrefix(rel, vfs.PartDir) {
			return nil
		}
		info, _ := d.Info()
		lines = append(lines, fmt.Sprintf("%s %d", rel, info.Size()))
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

const mib = 1 << 20

func TestMeshAndThirdPartyMove(t *testing.T) {
	a, b, c := mesh(t)
	want := writeRandom(t, filepath.Join(b.share, "big.bin"), 64*mib, 1)
	writeRandom(t, filepath.Join(a.share, "mine.txt"), 10, 2)
	before := snapshot(t, a.share)

	a.mustCLI("mv", "b:s/big.bin", "c:s/")

	if got := hashFile(t, filepath.Join(c.share, "big.bin")); got != want {
		t.Fatalf("hash on c %s, want %s", got, want)
	}
	if exists(filepath.Join(b.share, "big.bin")) {
		t.Fatal("source still on b after a completed move")
	}
	if after := snapshot(t, a.share); after != before {
		t.Fatalf("a's share changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// bigFile is large enough that the transfer is still running when the test
// reacts to the first progress.
const bigFile = 768 * mib

func TestKillDestMidTransferResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("moves a large file")
	}
	a, b, c := mesh(t)
	src := filepath.Join(b.share, "big.bin")
	want := writeRandom(t, src, bigFile, 3)

	id := a.mustCLI("mv", "b:s/big.bin", "c:s/", "--detach")
	c.waitJob(id, 2*time.Minute, "made progress", func(j transfer.Job) bool { return j.DoneBytes > 0 && !isDone(j) })
	c.kill()
	if !exists(src) {
		t.Fatal("source gone before the move completed")
	}
	c.restart()
	if r := c.cli("jobs", "watch", id); r.code != 0 {
		t.Fatalf("jobs watch: exit %d: %s %s", r.code, r.out, r.err)
	}
	c.waitJob(id, time.Minute, "done", isDone)
	if got := hashFile(t, filepath.Join(c.share, "big.bin")); got != want {
		t.Fatalf("hash after resume %s, want %s", got, want)
	}
	if exists(src) {
		t.Fatal("source not removed after the move completed")
	}
}

func TestKillSourceMidTransferWaits(t *testing.T) {
	if testing.Short() {
		t.Skip("moves a large file")
	}
	a, b, c := mesh(t)
	src := filepath.Join(b.share, "big.bin")
	want := writeRandom(t, src, bigFile, 4)

	id := a.mustCLI("mv", "b:s/big.bin", "c:s/", "--detach")
	c.waitJob(id, 2*time.Minute, "made progress", func(j transfer.Job) bool { return j.DoneBytes > 0 && !isDone(j) })
	b.kill()
	c.waitJob(id, time.Minute, "waiting_peer", func(j transfer.Job) bool { return j.State == transfer.StateWaitingPeer })
	if !exists(src) {
		t.Fatal("source gone while its host was dead")
	}
	b.restart()
	c.waitJob(id, 2*time.Minute, "done", isDone)
	if got := hashFile(t, filepath.Join(c.share, "big.bin")); got != want {
		t.Fatalf("hash after resume %s, want %s", got, want)
	}
	if exists(src) {
		t.Fatal("source not removed after the move completed")
	}
}

// TestKilledDaemonReleasesItsConfigDir covers the restart half of the kill
// tests on its own: after Process.Kill the lock is free and the same config
// directory starts again.
func TestKilledDaemonReleasesItsConfigDir(t *testing.T) {
	n := newNode(t, "solo")
	n.kill()
	n.restart()
	n.mustCLI("status")
}

func (n *node) davRequest(method, path string, body io.Reader, auth bool) (*http.Response, error) {
	tok, err := config.ControlToken(n.dir)
	if err != nil {
		n.t.Fatal(err)
	}
	u := url.URL{Scheme: "http", Host: n.dav, Path: path}
	req, _ := http.NewRequest(method, u.String(), body)
	if auth {
		req.SetBasicAuth("sugar-it", tok)
	}
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func TestWebDAVThroughDaemon(t *testing.T) {
	a, b, c := mesh(t)
	want := writeRandom(t, filepath.Join(c.share, "doc.bin"), 3*mib, 5)

	resp, err := a.davRequest("GET", "/c/s/doc.bin", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	io.Copy(h, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || hex.EncodeToString(h.Sum(nil)) != want {
		t.Fatalf("GET c's file through a: status %d, hash %x", resp.StatusCode, h.Sum(nil))
	}

	resp, err = a.davRequest("PUT", "/b/s/up.txt", strings.NewReader("hello through webdav"), true)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT status %d", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(b.share, "up.txt"))
	if err != nil || string(got) != "hello through webdav" {
		t.Fatalf("file on b: %q %v", got, err)
	}

	resp, err = a.davRequest("GET", "/c/s/doc.bin", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated GET: status %d, want 401", resp.StatusCode)
	}
}

func TestRemovedPeerLosesAccess(t *testing.T) {
	a, _, c := mesh(t)
	writeRandom(t, filepath.Join(a.share, "x.txt"), 5, 6)
	if r := c.cli("ls", "a:s"); r.code != 0 {
		t.Fatalf("before removal: exit %d %s", r.code, r.err)
	}
	a.mustCLI("peer", "remove", "c")
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := c.cli("ls", "a:s")
		if r.code != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("c can still list a:s after removal:\n%s", r.out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestManySmallFiles(t *testing.T) {
	a, b, _ := mesh(t)
	const count = 5000
	hashes := map[string]string{}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("d%02d/f%05d.dat", i%50, i)
		h := writeRandom(t, filepath.Join(b.share, "many", name), int64(100+i%3000), uint64(1000+i))
		hashes[name] = h
	}
	a.mustCLI("cp", "b:s/many", "a:s/")
	n := 0
	filepath.WalkDir(filepath.Join(a.share, "many"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if n != count {
		t.Fatalf("copied %d files, want %d", n, count)
	}
	i := 0
	for name, want := range hashes {
		if i++; i%25 != 0 {
			continue
		}
		if got := hashFile(t, filepath.Join(a.share, "many", name)); got != want {
			t.Errorf("%s: hash %s, want %s", name, got, want)
		}
	}
	if !exists(filepath.Join(b.share, "many", "d00", "f00000.dat")) {
		t.Fatal("a copy removed the source")
	}
}

func TestLargeListing(t *testing.T) {
	a, _, c := mesh(t)
	const count = 100000
	dir := filepath.Join(a.share, "huge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("entry-%06d.txt", i)))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	start := time.Now()
	out := c.mustCLI("ls", "--json", "a:s/huge")
	elapsed := time.Since(start)
	var es []vfs.Entry
	if err := json.Unmarshal([]byte(out), &es); err != nil {
		t.Fatal(err)
	}
	if len(es) != count {
		t.Fatalf("listed %d entries, want %d", len(es), count)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("listing took %s, want under 30s", elapsed)
	}
	t.Logf("listed %d entries in %s", count, elapsed)
}
