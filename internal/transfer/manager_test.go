package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"sugarintegration/internal/shares"
	"sugarintegration/internal/vfs"
)

const (
	self     = "self"
	peerB    = "peer-b"
	waitTime = 15 * time.Second
)

// rig runs a Manager over the env: share "s" lives on peerB and share "d" on
// self. Source and Dest can be replaced while it runs.
type rig struct {
	*env
	t       *testing.T
	dir     string
	mu      sync.Mutex
	srcF    vfs.FS // what Source returns for share "s"
	dstF    Dest   // what Dest returns for share "d"
	sro     bool   // source share read-only
	selfSrc vfs.FS // if set, what Source returns for this host's share "d"
	down    bool   // source peer offline

	// What the shares present; the env's own LocalFS unless named was called.
	srcView vfs.FS
	dstView Dest
	encoded bool // names on disk are diskName's

	opts   Options
	m      *Manager
	cancel context.CancelFunc
	done   chan error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	e := newEnv(t)
	r := &rig{env: e, t: t, dir: t.TempDir(), srcF: e.src, dstF: e.dst, srcView: e.src, dstView: e.dst}
	r.opts = Options{
		Dir: r.dir, SelfID: self, Concurrency: 2, GOOS: "linux",
		Source: func(peer, share string) (vfs.FS, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			switch {
			case peer == peerB && share == "s":
				if r.down {
					return nil, vfs.E(vfs.CodePeerOffline, "offline")
				}
				return r.srcF, nil
			case peer == self && share == "d":
				if r.selfSrc != nil {
					return r.selfSrc, nil
				}
				return r.dstF, nil
			}
			return nil, vfs.E(vfs.CodeShareNotFound, "no share %s/%s", peer, share)
		},
		SourceReadOnly: func(_ context.Context, peer, share string) (bool, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if peer == peerB && r.down {
				return false, vfs.E(vfs.CodePeerOffline, "offline")
			}
			return r.sro, nil
		},
		Dest: func(share string) (Dest, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if share != "d" {
				return nil, vfs.E(vfs.CodeShareNotFound, "no share")
			}
			return r.dstF, nil
		},
		RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond, SaveEvery: 5 * time.Millisecond,
	}
	t.Cleanup(r.stop)
	return r
}

// named makes both shares hold names no real directory on this OS may be able
// to, such as "A" and "a" or "a:b". Paths given to the rig's helpers stay as the
// job sees them.
func (r *rig) named() {
	r.encoded = true
	r.srcView, r.dstView = namedFS{r.src}, namedFS{r.dst}
	r.srcF, r.dstF = r.srcView, r.dstView
}

// onDisk is where a path given to the rig really is, relative to its share.
func (r *rig) onDisk(name string) string {
	if r.encoded {
		name = diskName(name)
	}
	return filepath.FromSlash(name)
}

func (r *rig) exists(dir, name string) bool { return r.env.exists(dir, r.onDisk(name)) }

// wantNoEntry looks in the listing: on Windows Lstat finds the device behind
// a name like CON.
func (r *rig) wantNoEntry(dir, name string) {
	r.t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, de := range des {
		if de.Name() == name {
			r.t.Errorf("%s exists in %s", name, dir)
		}
	}
}

func (r *rig) set(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn()
}

// start creates a Manager over the rig's Dir and runs it.
func (r *rig) start() *Manager {
	r.t.Helper()
	r.stop()
	m, err := NewManager(r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// Set first: a recovered job can reach a test hook as soon as Run starts.
	r.m, r.cancel, r.done = m, cancel, done
	go func() { done <- m.Run(ctx) }()
	return m
}

// stop kills the running Manager and waits for it to finish.
func (r *rig) stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(waitTime):
		r.t.Error("Run did not return after cancellation")
	}
	r.cancel = nil
}

func (r *rig) waitStopped() {
	r.t.Helper()
	select {
	case <-r.done:
		r.cancel = nil
	case <-time.After(waitTime):
		r.t.Fatal("Run did not return")
	}
}

func cpSpec(op, from, to string) Spec {
	return Spec{Op: op,
		Src:        Endpoint{Peer: peerB, Share: "s", Path: from},
		Dst:        Endpoint{Peer: self, Share: "d", Path: to},
		OnConflict: ConflictFail}
}

func (r *rig) submit(s Spec) string {
	r.t.Helper()
	j, err := r.m.Submit("origin", s)
	if err != nil {
		r.t.Fatalf("Submit: %v", err)
	}
	return j.ID
}

func waitState(t *testing.T, m *Manager, id string, want ...State) Job {
	t.Helper()
	deadline := time.Now().Add(waitTime)
	var j Job
	for time.Now().Before(deadline) {
		var err error
		if j, err = m.Get(id); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(want, j.State) {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job never reached %v: %+v", want, j)
	return j
}

func (r *rig) waitDone(id string) Job {
	r.t.Helper()
	j := waitState(r.t, r.m, id, StateDone, StatePartial, StateFailed, StateCanceled)
	if j.State != StateDone {
		r.t.Fatalf("job ended %s: %+v", j.State, j)
	}
	return j
}

func (r *rig) tree(dir string, files map[string]string) {
	r.t.Helper()
	for name, data := range files {
		p := filepath.Join(dir, r.onDisk(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *rig) wantFile(dir, name, data string) {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, r.onDisk(name)))
	if err != nil {
		r.t.Errorf("%s: %v", name, err)
	} else if string(b) != data {
		r.t.Errorf("%s = %q, want %q", name, b, data)
	}
}

func (r *rig) wantGone(dir, name string) {
	r.t.Helper()
	if r.exists(dir, name) {
		r.t.Errorf("%s still exists", name)
	}
}

func (r *rig) doneLogLines(id string) int {
	b, err := os.ReadFile(filepath.Join(r.dir, "jobs", id, "done.log"))
	if err != nil {
		return 0
	}
	return bytes.Count(b, []byte("\n"))
}

func TestNewManagerConcurrencyDefault(t *testing.T) {
	for _, n := range []int{0, -1} {
		r := newRig(t)
		r.opts.Concurrency = n
		m, err := NewManager(r.opts)
		if err != nil {
			t.Fatal(err)
		}
		if m.o.Concurrency != 2 {
			t.Errorf("Concurrency %d became %d", n, m.o.Concurrency)
		}
	}
}

func TestCopyTree(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"top/a.txt": "aa", "top/sub/b.txt": "bbb", "top/sub/deep/c.txt": "c"})
	if err := os.MkdirAll(filepath.Join(r.srcDir, "top/empty/inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.start()
	j := r.waitDone(r.submit(cpSpec("copy", "top", "")))
	r.wantFile(r.dstDir, "top/a.txt", "aa")
	r.wantFile(r.dstDir, "top/sub/b.txt", "bbb")
	r.wantFile(r.dstDir, "top/sub/deep/c.txt", "c")
	if fi, err := os.Stat(filepath.Join(r.dstDir, "top/empty/inner")); err != nil || !fi.IsDir() {
		t.Errorf("empty directory not created: %v", err)
	}
	r.wantFile(r.srcDir, "top/a.txt", "aa")
	r.wantFile(r.srcDir, "top/sub/deep/c.txt", "c")
	if j.TotalFiles != 3 || j.DoneFiles != 3 || j.TotalBytes != 6 || j.DoneBytes != 6 || j.FailedFiles != 0 {
		t.Errorf("counters: %+v", j)
	}
	if j.Host != self || j.Origin != "origin" {
		t.Errorf("host/origin: %+v", j)
	}
	r.noParts(t)
}

func TestCopyCreatesDestinationParents(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "x"})
	r.start()
	r.waitDone(r.submit(cpSpec("copy", "f", "new/dirs/g")))
	r.wantFile(r.dstDir, "new/dirs/g", "x")
	r.waitDone(r.submit(cpSpec("copy", "f", "other/place/")))
	r.wantFile(r.dstDir, "other/place/f", "x")
}

func TestCopyShareRoot(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"a": "1", "d/b": "2"})
	r.start()
	r.waitDone(r.submit(cpSpec("move", "", "into")))
	r.wantFile(r.dstDir, "into/a", "1")
	r.wantFile(r.dstDir, "into/d/b", "2")
	r.wantGone(r.srcDir, "a")
	r.wantGone(r.srcDir, "d")
	if _, err := os.Stat(r.srcDir); err != nil {
		t.Errorf("share root removed: %v", err)
	}
}

func TestMoveTreeRemovesSource(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"top/a": "aa", "top/sub/b": "bbb", "keep": "k"})
	if err := os.Mkdir(filepath.Join(r.srcDir, "top/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.start()
	j := r.waitDone(r.submit(cpSpec("move", "top", "")))
	r.wantFile(r.dstDir, "top/a", "aa")
	r.wantFile(r.dstDir, "top/sub/b", "bbb")
	if !r.exists(r.dstDir, "top/empty") {
		t.Error("empty dir not moved")
	}
	r.wantGone(r.srcDir, "top")
	r.wantFile(r.srcDir, "keep", "k")
	if j.DoneFiles != 2 {
		t.Errorf("counters: %+v", j)
	}
}

func TestSubmitToReadOnlyDest(t *testing.T) {
	r := newRig(t)
	ro, err := openRO(r.dstDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	r.dstF = ro
	r.tree(r.srcDir, map[string]string{"f": "x"})
	r.start()
	_, err = r.m.Submit("o", cpSpec("copy", "f", "g"))
	wantCode(t, err, vfs.CodeReadOnly)
	if js := r.m.List(""); len(js) != 0 {
		t.Errorf("job created: %+v", js)
	}
	if ents, _ := os.ReadDir(filepath.Join(r.dir, "jobs")); len(ents) != 0 {
		t.Errorf("job persisted: %v", ents)
	}
}

func TestSubmitValidatesWhenReachable(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "x"})
	r.start()
	_, err := r.m.Submit("o", cpSpec("copy", "missing", "g"))
	wantCode(t, err, vfs.CodeNotFound)
	r.set(func() { r.sro = true })
	_, err = r.m.Submit("o", cpSpec("move", "f", "g"))
	wantCode(t, err, vfs.CodeReadOnly)
	s := cpSpec("copy", "f", "g")
	s.Dst.Peer = "someone-else"
	_, err = r.m.Submit("o", s)
	wantCode(t, err, vfs.CodeInvalidRequest)
	s = cpSpec("copy", "f", "g")
	s.Dst.Share = "nope"
	_, err = r.m.Submit("o", s)
	wantCode(t, err, vfs.CodeShareNotFound)
	if js := r.m.List(""); len(js) != 0 {
		t.Errorf("jobs created: %+v", js)
	}
}

func TestSubmitDefaultsConflictToFail(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "new"})
	r.tree(r.dstDir, map[string]string{"f": "old"})
	r.start()
	s := cpSpec("copy", "f", "f")
	s.OnConflict = ""
	id := r.submit(s)
	j := waitState(t, r.m, id, StatePartial)
	if j.Spec.OnConflict != ConflictFail || j.Failures[0].Code != vfs.CodeConflict {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "f", "old")
}

func TestSubmitWhileSourceOffline(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "x", "g": "y"})
	r.down = true
	r.start()

	good := r.submit(cpSpec("copy", "f", "f"))
	bad := r.submit(cpSpec("move", "g", "g"))
	waitState(t, r.m, good, StateWaitingPeer)
	waitState(t, r.m, bad, StateWaitingPeer)

	// Back online, but the source share turned out to be read-only: a move
	// breaks the rule and fails; a copy goes ahead.
	r.set(func() { r.down = false; r.sro = true })
	r.waitDone(good)
	r.wantFile(r.dstDir, "f", "x")
	j := waitState(t, r.m, bad, StateFailed, StateDone, StatePartial)
	if j.State != StateFailed || j.Error == nil || j.Error.Code != vfs.CodeReadOnly {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "g", "y")
	r.wantGone(r.dstDir, "g")
}

func TestSelfMoveViaResolvedDestinationRejected(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"a/x": "1", "p/a/y": "2"})
	r.start()
	s := Spec{Op: "move", Src: Endpoint{Peer: self, Share: "d", Path: "a"}, Dst: Endpoint{Peer: self, Share: "d", Path: ""}}
	_, err := r.m.Submit("o", s) // resolves to a -> a
	wantCode(t, err, vfs.CodeInvalidRequest)
	s = Spec{Op: "copy", Src: Endpoint{Peer: self, Share: "d", Path: "p/a"}, Dst: Endpoint{Peer: self, Share: "d", Path: "p"}}
	_, err = r.m.Submit("o", s) // resolves to p/a -> p/a
	wantCode(t, err, vfs.CodeInvalidRequest)
	r.wantFile(r.dstDir, "a/x", "1")
	r.wantFile(r.dstDir, "p/a/y", "2")
}

func TestSelfMoveCaseInsensitiveRejected(t *testing.T) {
	r := newRig(t)
	r.opts.GOOS = "darwin"
	r.tree(r.dstDir, map[string]string{"a/x": "1"})
	r.start()
	s := Spec{Op: "move", Src: Endpoint{Peer: self, Share: "d", Path: "a"}, Dst: Endpoint{Peer: self, Share: "d", Path: "A/b"}}
	_, err := r.m.Submit("o", s)
	wantCode(t, err, vfs.CodeInvalidRequest)
}

func TestMoveKeepsFailedSource(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/ok": "ok", "t/sub/clash": "new", "t/sub2/fine": "fine"})
	r.tree(r.dstDir, map[string]string{"t/sub/clash": "old"})
	r.start()
	id := r.submit(cpSpec("move", "t", ""))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.FailedFiles != 1 || j.DoneFiles != 2 {
		t.Fatalf("%+v", j)
	}
	if len(j.Failures) != 1 || j.Failures[0].Code != vfs.CodeConflict || j.Failures[0].Path != "t/sub/clash" {
		t.Errorf("failures: %+v", j.Failures)
	}
	r.wantFile(r.srcDir, "t/sub/clash", "new")
	r.wantFile(r.dstDir, "t/sub/clash", "old")
	r.wantGone(r.srcDir, "t/ok")
	r.wantGone(r.srcDir, "t/sub2")
	r.wantFile(r.dstDir, "t/ok", "ok")
}

func TestMoveSkipKeepsSource(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/a": "new", "t/b": "b"})
	r.tree(r.dstDir, map[string]string{"t/a": "old"})
	r.start()
	s := cpSpec("move", "t", "")
	s.OnConflict = ConflictSkip
	j := r.waitDone(r.submit(s))
	if j.SkippedFiles != 1 || j.FailedFiles != 0 || j.DoneFiles != 1 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "t/a", "new")
	r.wantFile(r.dstDir, "t/a", "old")
	r.wantGone(r.srcDir, "t/b")
}

func TestMoveDropNeverDeletesEarly(t *testing.T) {
	r := newRig(t)
	data := randBytes(1 << 20)
	r.write(t, r.srcDir, "f", data)
	f := newFault(r.src)
	f.setDrop(300 << 10)
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	waitState(t, r.m, id, StateWaitingPeer)
	f.setDrop(-1)
	r.waitDone(id)

	calls := f.callLog()
	hash, rm := slices.Index(calls, "hash f"), slices.Index(calls, "remove f")
	if hash < 0 || rm < 0 || rm < hash {
		t.Errorf("Remove must follow Hash: %v", calls)
	}
	if n := countOf(calls, "remove f"); n != 1 {
		t.Errorf("removed %d times", n)
	}
	r.wantGone(r.srcDir, "f")
	if r.read(t, r.dstDir, "f") != string(data) {
		t.Error("content differs")
	}
}

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

func TestMoveHashFailureKeepsSource(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "data"})
	f := newFault(r.src)
	f.hash = "deadbeef"
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	j := waitState(t, r.m, id, StatePartial)
	if j.Failures[0].Code != vfs.CodeHashMismatch {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "data")
	r.wantGone(r.dstDir, "f")
	if n := countOf(f.callLog(), "remove f"); n != 0 {
		t.Error("source removal attempted")
	}
}

func TestMoveSourceEditedAfterVerifyKept(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "data"})
	// Edit the source after the commit, right before deletion would run.
	d := &hookDest{Dest: r.dst, afterCommit: func() {
		r.tree(r.srcDir, map[string]string{"f": "edited, must survive"})
	}}
	r.dstF = d
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeChanged {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "edited, must survive")
	r.wantFile(r.dstDir, "f", "data")
}

// hookDest runs afterCommit once the part is committed; err, if set, is then
// returned as though the commit call had been interrupted.
type hookDest struct {
	Dest
	once        sync.Once
	afterCommit func()
	err         error
}

func (d *hookDest) CommitPart(key, dst string, mt time.Time, mode uint32) error {
	if err := d.Dest.CommitPart(key, dst, mt, mode); err != nil {
		return err
	}
	var ret error
	d.once.Do(func() {
		d.afterCommit()
		ret = d.err
	})
	return ret
}

func sameShare(op, from, to string, c Conflict) Spec {
	return Spec{Op: op,
		Src:        Endpoint{Peer: self, Share: "d", Path: from},
		Dst:        Endpoint{Peer: self, Share: "d", Path: to},
		OnConflict: c}
}

// faultDest records calls on a share used as both source and destination.
type faultDest struct {
	Dest
	f *faultFS
}

func (d faultDest) Open(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error) {
	return d.f.Open(ctx, p, off, etag)
}
func (d faultDest) Rename(ctx context.Context, from, to string) error {
	return d.f.Rename(ctx, from, to)
}

func TestMoveSameShareIsRename(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"t/a": "aa", "t/sub/b": "bbb"})
	if err := os.Mkdir(filepath.Join(r.dstDir, "t/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFault(r.dst)
	r.dstF = faultDest{r.dst, f}
	before, _ := os.Stat(filepath.Join(r.dstDir, "t"))
	// On Windows os.Stat's identity is read from the path at the first
	// SameFile, and after the rename that path is gone.
	os.SameFile(before, before)
	r.start()
	j := r.waitDone(r.submit(sameShare("move", "t", "new/parent/u", ConflictFail)))
	calls := f.callLog()
	for _, c := range calls {
		if strings.HasPrefix(c, "open ") {
			t.Errorf("Open called: %v", calls)
		}
	}
	if n := len(calls); n != 1 || calls[0] != "rename t" {
		t.Errorf("want one rename of the whole tree, got %v", calls)
	}
	r.wantFile(r.dstDir, "new/parent/u/a", "aa")
	r.wantFile(r.dstDir, "new/parent/u/sub/b", "bbb")
	r.wantGone(r.dstDir, "t")
	after, _ := os.Stat(filepath.Join(r.dstDir, "new/parent/u"))
	if !os.SameFile(before, after) {
		t.Error("directory was not renamed")
	}
	if j.TotalFiles != 2 || j.DoneFiles != 2 || j.DoneBytes != 5 {
		t.Errorf("counters %+v", j)
	}
}

func TestMoveSameShareConflictFail(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"a": "new", "b": "old"})
	r.start()
	id := r.submit(sameShare("move", "a", "b", ConflictFail))
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeConflict {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "a", "new")
	r.wantFile(r.dstDir, "b", "old")
}

func TestMoveSameShareConflictOverwrite(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"a": "new", "b": "old"})
	r.start()
	j := r.waitDone(r.submit(sameShare("move", "a", "b", ConflictOverwrite)))
	r.wantFile(r.dstDir, "b", "new")
	r.wantGone(r.dstDir, "a")
	if j.DoneFiles != 1 || j.FailedFiles != 0 {
		t.Errorf("%+v", j)
	}
}

func TestMoveSameShareConflictSkipAndRename(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"a": "A", "b": "B", "c": "C"})
	r.start()
	j := r.waitDone(r.submit(sameShare("move", "a", "c", ConflictSkip)))
	if j.SkippedFiles != 1 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "a", "A")
	r.wantFile(r.dstDir, "c", "C")
	r.waitDone(r.submit(sameShare("move", "b", "c", ConflictRename)))
	r.wantFile(r.dstDir, "c", "C")
	r.wantFile(r.dstDir, "c (1)", "B")
	r.wantGone(r.dstDir, "b")
}

// A directory merged into an existing one renames each free name without
// reading it and applies the policy to the names that are taken.
func TestMoveSameShareMergesIntoExistingDir(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"s/a": "A", "s/sub/b": "B", "t/s/a": "old"})
	f := newFault(r.dst)
	r.dstF = faultDest{r.dst, f}
	r.start()
	id := r.submit(sameShare("move", "s", "t", ConflictFail)) // t exists: t/s
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.DoneFiles != 1 || j.FailedFiles != 1 {
		t.Fatalf("%+v", j)
	}
	r.wantFile(r.dstDir, "t/s/a", "old")
	r.wantFile(r.dstDir, "s/a", "A")
	r.wantFile(r.dstDir, "t/s/sub/b", "B")
	r.wantGone(r.dstDir, "s/sub")
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "open ") {
			t.Errorf("Open called: %v", f.callLog())
		}
	}
}

func TestSymlinkInTreeSkipped(t *testing.T) {
	for _, op := range []string{"copy", "move"} {
		t.Run(op, func(t *testing.T) {
			r := newRig(t)
			outside := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
				t.Fatal(err)
			}
			r.tree(r.srcDir, map[string]string{"t/a": "aa"})
			if err := os.Symlink(outside, filepath.Join(r.srcDir, "t/out")); err != nil {
				t.Skip(err)
			}
			if err := os.Symlink("..", filepath.Join(r.srcDir, "t/loop")); err != nil {
				t.Fatal(err)
			}
			r.start()
			j := r.waitDone(r.submit(cpSpec(op, "t", "")))
			if j.SkippedFiles != 2 || j.DoneFiles != 1 || j.FailedFiles != 0 {
				t.Errorf("%+v", j)
			}
			r.wantFile(r.dstDir, "t/a", "aa")
			r.wantGone(r.dstDir, "t/out")
			r.wantGone(r.dstDir, "t/loop")
			if op == "move" { // skipped sources stay, so their directory stays
				for _, n := range []string{"t/out", "t/loop"} {
					if !r.exists(r.srcDir, n) {
						t.Errorf("%s removed", n)
					}
				}
			}
		})
	}
}

func TestTopLevelSymlinkSkipped(t *testing.T) {
	r := newRig(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(r.srcDir, "l")); err != nil {
		t.Skip(err)
	}
	r.start()
	j := r.waitDone(r.submit(cpSpec("move", "l", "l")))
	if j.SkippedFiles != 1 {
		t.Errorf("%+v", j)
	}
	r.wantGone(r.dstDir, "l")
	if !r.exists(r.srcDir, "l") {
		t.Error("source link removed")
	}
}

func TestSameShareSymlinkSkipped(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"t/a": "aa"})
	if err := os.Symlink("a", filepath.Join(r.dstDir, "t/l")); err != nil {
		t.Skip(err)
	}
	r.start()
	j := r.waitDone(r.submit(sameShare("move", "t", "u", ConflictFail)))
	if j.SkippedFiles != 1 || j.DoneFiles != 1 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "u/a", "aa")
	r.wantGone(r.dstDir, "u/l")
	if !r.exists(r.dstDir, "t/l") {
		t.Error("link removed from source")
	}
}

func TestPeerOfflineWaitsThenResumes(t *testing.T) {
	r := newRig(t)
	data := randBytes(2 << 20)
	r.write(t, r.srcDir, "big", data)
	f := newFault(r.src)
	f.setDrop(256 << 10)
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("copy", "big", "big"))
	waitState(t, r.m, id, StateWaitingPeer)
	r.set(func() { r.down = true })
	f.setDrop(-1)
	time.Sleep(30 * time.Millisecond)
	if j, _ := r.m.Get(id); j.State != StateWaitingPeer {
		t.Errorf("state while offline: %s", j.State)
	}
	r.set(func() { r.down = false })
	r.waitDone(id)
	if r.read(t, r.dstDir, "big") != string(data) {
		t.Error("content differs")
	}
	r.noParts(t)
	if f.offsets[0] != 0 || slices.Max(f.offsets) == 0 {
		t.Errorf("did not resume: %v", f.offsets)
	}
}

func TestWaitingPeerDoesNotHoldSlot(t *testing.T) {
	r := newRig(t)
	r.opts.Concurrency = 1
	r.tree(r.srcDir, map[string]string{"f": "x"})
	r.tree(r.dstDir, map[string]string{"inner": "y"})
	r.down = true
	r.start()
	waiting := r.submit(cpSpec("copy", "f", "f"))
	waitState(t, r.m, waiting, StateWaitingPeer)
	r.waitDone(r.submit(sameShare("copy", "inner", "inner2", ConflictFail)))
	r.wantFile(r.dstDir, "inner2", "y")
}

func TestDoneBytesNeverExceedsFileSize(t *testing.T) {
	r := newRig(t)
	size := int64(1 << 20)
	r.write(t, r.srcDir, "f", randBytes(int(size)))
	f := newFault(r.src)
	f.hookAt = size / 2
	r.srcF = f
	var over atomic.Int64
	sample := func(m *Manager) {
		for _, j := range m.List("") {
			if j.DoneBytes > size {
				over.Store(j.DoneBytes)
			}
		}
	}
	var fired atomic.Bool
	f.hook = func() {
		sample(r.m)
		if !fired.Swap(true) { // source changes mid-stream: Fetch restarts, delta < 0
			now := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(r.srcDir, "f"), now, now); err != nil {
				t.Error(err)
			}
		}
	}
	r.start()
	id := r.submit(cpSpec("copy", "f", "f"))
	j := r.waitDone(id)
	if over.Load() != 0 {
		t.Errorf("DoneBytes reached %d of %d", over.Load(), size)
	}
	if j.DoneBytes != size || j.TotalBytes != size {
		t.Errorf("%+v", j)
	}
}

func TestDoneBytesAfterOfflineRetries(t *testing.T) {
	r := newRig(t)
	size := int64(2 << 20)
	r.write(t, r.srcDir, "f", randBytes(int(size)))
	f := newFault(r.src)
	f.setDrop(128 << 10) // many short streams, each resuming the part
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("copy", "f", "f"))
	deadline := time.Now().Add(waitTime)
	for {
		j, err := r.m.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.DoneBytes > size {
			t.Fatalf("DoneBytes %d exceeds %d", j.DoneBytes, size)
		}
		if j.State == StateDone {
			if j.DoneBytes != size {
				t.Errorf("final DoneBytes %d", j.DoneBytes)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not finished: %+v", j)
		}
		time.Sleep(time.Millisecond)
	}
}

// A file that fails partway and keeps the job going must not leave its bytes
// in DoneBytes.
func TestDoneBytesRewoundOnItemFailure(t *testing.T) {
	r := newRig(t)
	r.write(t, r.srcDir, "f", randBytes(1<<20))
	f := newFault(r.src)
	f.setDrop(512 << 10)
	f.dropCode = vfs.CodeInternal
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("copy", "f", "f"))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.DoneBytes != 0 || j.FailedFiles != 1 {
		t.Errorf("%+v", j)
	}
	r.noParts(t)
}

func TestRestartResumes(t *testing.T) {
	r := newRig(t)
	big := randBytes(2 << 20)
	r.tree(r.srcDir, map[string]string{"t/a": "aa", "t/b": "bb"})
	r.write(t, r.srcDir, "t/c", big)
	f := newFault(r.src)
	f.hookPath, f.hookAt = "t/c", 1<<20
	r.srcF = f
	r.start()
	f.hook = func() { r.cancel() }
	id := r.submit(cpSpec("copy", "t", ""))
	r.waitStopped()
	if n := r.doneLogLines(id); n != 2 {
		t.Fatalf("done.log has %d entries", n)
	}
	var onDisk record
	b, err := os.ReadFile(filepath.Join(r.dir, "jobs", id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.State != StateRunning {
		t.Errorf("state on disk: %s", onDisk.State)
	}

	f2 := newFault(r.src)
	r.srcF = f2
	r.start()
	j := r.waitDone(id)
	if got := f2.openedPaths(); !slices.Equal(got, []string{"t/c"}) {
		t.Errorf("re-fetched: %v", got)
	}
	if f2.offsets[0] == 0 {
		t.Error("big file restarted from zero")
	}
	r.wantFile(r.dstDir, "t/a", "aa")
	if r.read(t, r.dstDir, "t/c") != string(big) {
		t.Error("content differs")
	}
	if j.DoneFiles != 3 || j.TotalFiles != 3 || j.DoneBytes != int64(len(big))+4 {
		t.Errorf("%+v", j)
	}
	r.noParts(t)
}

// "a -> x" with no x means x IS the copy. When x exists after the restart the
// job must not re-resolve to x/a.
func TestRestartKeepsResolvedDestination(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"a/1": "one"})
	r.write(t, r.srcDir, "a/2", randBytes(2<<20))
	f := newFault(r.src)
	f.hookPath, f.hookAt = "a/2", 1<<20
	r.srcF = f
	r.start()
	f.hook = func() { r.cancel() }
	id := r.submit(cpSpec("copy", "a", "x"))
	r.waitStopped()
	r.srcF = r.src
	r.start()
	r.waitDone(id)
	r.wantFile(r.dstDir, "x/1", "one")
	if !r.exists(r.dstDir, "x/2") || r.exists(r.dstDir, "x/a") {
		t.Errorf("destination re-resolved: %v", listTree(r.dstDir))
	}
}

func listTree(dir string) []string {
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		out = append(out, p)
		return nil
	})
	return out
}

// crashAfterCommit runs the spec until the first CommitPart has renamed the
// file into place, then kills the Manager before done.log is written.
func (r *rig) crashAfterCommit(s Spec) string {
	r.t.Helper()
	d := &hookDest{Dest: r.dst, err: vfs.E(vfs.CodeCanceled, "killed"), afterCommit: func() { r.cancel() }}
	r.dstF = d
	r.start()
	id := r.submit(s)
	r.waitStopped()
	if n := r.doneLogLines(id); n != 0 {
		r.t.Fatalf("done.log written before the simulated kill: %d", n)
	}
	r.dstF = r.dst
	return id
}

func TestCrashAfterCommitMoveFinishes(t *testing.T) {
	for _, c := range []Conflict{ConflictFail, ConflictSkip, ConflictOverwrite, ConflictRename} {
		t.Run(string(c), func(t *testing.T) {
			r := newRig(t)
			r.tree(r.srcDir, map[string]string{"d/f": "payload"})
			s := cpSpec("move", "d", "")
			s.OnConflict = c
			id := r.crashAfterCommit(s)
			r.wantFile(r.srcDir, "d/f", "payload") // the kill left the source
			r.wantFile(r.dstDir, "d/f", "payload")

			f := newFault(r.src)
			r.srcF = f
			r.start()
			j := r.waitDone(id)
			if j.DoneFiles != 1 || j.SkippedFiles != 0 || j.FailedFiles != 0 || j.DoneBytes != 7 || len(j.Failures) != 0 {
				t.Errorf("%+v", j)
			}
			r.wantGone(r.srcDir, "d") // source must not linger
			r.wantFile(r.dstDir, "d/f", "payload")
			r.wantGone(r.dstDir, "d/f (1)")
			if got := f.openedPaths(); len(got) != 0 {
				t.Errorf("transferred again: %v", got)
			}
			r.noParts(t)
			if r.doneLogLines(id) != 1 {
				t.Error("recovered file not logged")
			}
		})
	}
}

func TestCrashAfterCommitCopyFinishes(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "payload"})
	id := r.crashAfterCommit(cpSpec("copy", "f", "f"))
	r.start()
	j := r.waitDone(id)
	if j.DoneFiles != 1 || j.FailedFiles != 0 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "payload")
	r.wantFile(r.dstDir, "f", "payload")
}

// With rename, the committed file sits under the "(1)" name beside the file
// that caused the conflict. Recovery must find it there, not make "(2)".
func TestCrashAfterCommitRenamePolicyNoDuplicate(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f.txt": "payload"})
	r.tree(r.dstDir, map[string]string{"f.txt": "other", "f (1).txt": "other too"})
	s := cpSpec("move", "f.txt", "f.txt")
	s.OnConflict = ConflictRename
	id := r.crashAfterCommit(s)
	r.wantFile(r.dstDir, "f (2).txt", "payload")
	r.start()
	j := r.waitDone(id)
	if j.DoneFiles != 1 || j.FailedFiles != 0 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "f.txt", "other")
	r.wantFile(r.dstDir, "f (1).txt", "other too")
	r.wantFile(r.dstDir, "f (2).txt", "payload")
	r.wantGone(r.dstDir, "f (3).txt")
	r.wantGone(r.srcDir, "f.txt")
}

// Overwrite replaced a different file; recovery must not transfer again.
func TestCrashAfterCommitOverwriteNotRefetched(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "payload"})
	r.tree(r.dstDir, map[string]string{"f": "previous"})
	s := cpSpec("copy", "f", "f")
	s.OnConflict = ConflictOverwrite
	id := r.crashAfterCommit(s)
	f := newFault(r.src)
	r.srcF = f
	r.start()
	r.waitDone(id)
	if got := f.openedPaths(); len(got) != 0 {
		t.Errorf("transferred again: %v", got)
	}
	r.wantFile(r.dstDir, "f", "payload")
}

// A kill after done.log but before the source removal leaves a logged file
// whose source exists; resume removes the source.
func TestCrashAfterDoneLogRemovesSource(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"d/f": "payload", "d/g": "second"})
	f := newFault(r.src)
	var once sync.Once
	f.onRemove = func(string) error {
		var err error
		once.Do(func() {
			r.cancel()
			err = vfs.E(vfs.CodeCanceled, "killed")
		})
		return err
	}
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("move", "d", ""))
	r.waitStopped()
	if r.doneLogLines(id) != 1 {
		t.Fatalf("done.log: %d", r.doneLogLines(id))
	}
	r.wantFile(r.srcDir, "d/f", "payload")

	f2 := newFault(r.src)
	r.srcF = f2
	r.start()
	j := r.waitDone(id)
	r.wantGone(r.srcDir, "d")
	r.wantFile(r.dstDir, "d/f", "payload")
	r.wantFile(r.dstDir, "d/g", "second")
	if slices.Contains(f2.openedPaths(), "d/f") {
		t.Error("logged file transferred again")
	}
	if j.DoneFiles != 2 || j.TotalFiles != 2 || j.DoneBytes != 13 {
		t.Errorf("%+v", j)
	}
}

// crashAfterDoneLog leaves a logged move whose source still exists.
func (r *rig) crashAfterDoneLog() string {
	r.t.Helper()
	r.tree(r.srcDir, map[string]string{"f": "payload"})
	f := newFault(r.src)
	f.onRemove = func(string) error {
		r.cancel()
		return vfs.E(vfs.CodeCanceled, "killed")
	}
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	r.waitStopped()
	r.srcF = r.src
	return id
}

// Between the kill and the restart the user edited the source. The logged
// file is no longer what was transferred, so it must survive.
func TestLoggedMoveKeepsEditedSource(t *testing.T) {
	r := newRig(t)
	id := r.crashAfterDoneLog()
	r.tree(r.srcDir, map[string]string{"f": "edited after the kill"})
	r.start()
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeChanged {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "edited after the kill")
	r.wantFile(r.dstDir, "f", "payload")
}

// The destination vanished between the kill and the restart: the source is
// now the only copy.
func TestLoggedMoveKeepsSourceWhenDestGone(t *testing.T) {
	r := newRig(t)
	id := r.crashAfterDoneLog()
	if err := os.Remove(filepath.Join(r.dstDir, "f")); err != nil {
		t.Fatal(err)
	}
	r.start()
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "payload")
}

// A move explains a missing source once items are logged; a copy does not.
func TestCopySourceGoneOnRestartFails(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/a": "aa"})
	r.write(t, r.srcDir, "t/b", randBytes(1<<20))
	f := newFault(r.src)
	f.hookPath, f.hookAt = "t/b", 512<<10
	r.srcF = f
	r.start()
	f.hook = func() { r.cancel() }
	id := r.submit(cpSpec("copy", "t", ""))
	r.waitStopped()
	if err := os.RemoveAll(filepath.Join(r.srcDir, "t")); err != nil {
		t.Fatal(err)
	}
	r.srcF = r.src
	r.start()
	j := waitState(t, r.m, id, StateFailed, StateDone, StatePartial)
	if j.State != StateFailed || j.Error == nil || j.Error.Code != vfs.CodeNotFound {
		t.Errorf("%+v", j)
	}
}

func TestCrashWithDifferentDestStillConflicts(t *testing.T) {
	// Same size and mtime but different bytes: the hash decides.
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "payload"})
	id := r.crashAfterCommit(cpSpec("move", "f", "f"))
	r.tree(r.dstDir, map[string]string{"f": "PAYLOAD"})
	src, _ := os.Stat(filepath.Join(r.srcDir, "f"))
	if err := os.Chtimes(filepath.Join(r.dstDir, "f"), src.ModTime(), src.ModTime()); err != nil {
		t.Fatal(err)
	}
	r.start()
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeConflict {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "payload")
	r.wantFile(r.dstDir, "f", "PAYLOAD")
}

func TestCrashWithDifferentMtimeStillConflicts(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "payload"})
	id := r.crashAfterCommit(cpSpec("move", "f", "f"))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(r.dstDir, "f"), old, old); err != nil {
		t.Fatal(err)
	}
	r.start()
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeConflict {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "payload")
}

// The crash rule is for recovered jobs only: a new job with an identical file
// in the way follows the conflict policy and keeps its source.
func TestFreshJobIdenticalFileStillConflicts(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "payload"})
	r.tree(r.dstDir, map[string]string{"f": "payload"})
	src, _ := os.Stat(filepath.Join(r.srcDir, "f"))
	if err := os.Chtimes(filepath.Join(r.dstDir, "f"), src.ModTime(), src.ModTime()); err != nil {
		t.Fatal(err)
	}
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeConflict {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "payload")
}

// A kill right after a whole-tree rename: source gone, destination present.
func TestCrashAfterSameShareRename(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"t/a": "aa"})
	rd := &renameKillDest{Dest: r.dst, kill: func() { r.cancel() }}
	r.dstF = rd
	r.start()
	id := r.submit(sameShare("move", "t", "u", ConflictFail))
	r.waitStopped()
	r.wantGone(r.dstDir, "t")
	r.dstF = r.dst
	r.start()
	j := r.waitDone(id)
	r.wantFile(r.dstDir, "u/a", "aa")
	r.wantGone(r.dstDir, "u/t")
	if j.DoneFiles != 1 || j.TotalFiles != 1 {
		t.Errorf("%+v", j)
	}
}

type renameKillDest struct {
	Dest
	kill func()
}

func (d *renameKillDest) Rename(ctx context.Context, from, to string) error {
	if err := d.Dest.Rename(ctx, from, to); err != nil {
		return err
	}
	d.kill()
	return vfs.E(vfs.CodeCanceled, "killed")
}

func TestInvalidParentNameFailsItem(t *testing.T) {
	r := newRig(t)
	r.named()
	r.opts.GOOS = "windows"
	r.tree(r.srcDir, map[string]string{"t/a:b/f": "x", "t/a:b/deeper/g": "y", "t/ok": "ok", "t/sub/fine": "fine"})
	if err := os.Mkdir(filepath.Join(r.srcDir, r.onDisk("t/e:mpty")), 0o755); err != nil {
		t.Fatal(err)
	}
	r.start()
	id := r.submit(cpSpec("move", "t", ""))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.FailedFiles != 3 || j.DoneFiles != 2 {
		t.Fatalf("%+v", j)
	}
	for _, f := range j.Failures {
		if f.Code != vfs.CodeInvalidName {
			t.Errorf("%+v", f)
		}
	}
	r.wantFile(r.srcDir, "t/a:b/f", "x")
	r.wantFile(r.srcDir, "t/a:b/deeper/g", "y")
	if !r.exists(r.srcDir, "t/e:mpty") {
		t.Error("empty directory with an invalid name removed from the source")
	}
	r.wantGone(r.dstDir, "t/a:b")
	r.wantGone(r.dstDir, "t/e:mpty")
	r.wantFile(r.dstDir, "t/ok", "ok")
	r.wantFile(r.dstDir, "t/sub/fine", "fine")
	r.wantGone(r.srcDir, "t/ok")
}

func TestInvalidNameInDestinationPath(t *testing.T) {
	r := newRig(t)
	r.opts.GOOS = "windows"
	r.tree(r.srcDir, map[string]string{"f": "x"})
	r.start()
	id := r.submit(cpSpec("move", "f", "CON/f"))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeInvalidName {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "x")
	r.wantNoEntry(r.dstDir, "CON")
}

func TestSameShareInvalidNameKeepsSource(t *testing.T) {
	r := newRig(t)
	r.named()
	r.opts.GOOS = "windows"
	r.tree(r.dstDir, map[string]string{"t/a:b/f": "x", "t/ok": "ok"})
	r.start()
	id := r.submit(sameShare("move", "t", "u", ConflictFail))
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.FailedFiles != 1 || j.Failures[0].Code != vfs.CodeInvalidName {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "t/a:b/f", "x")
	r.wantFile(r.dstDir, "u/ok", "ok")
	r.wantGone(r.dstDir, "u/a:b")
}

func TestDirectoryBlockedByFile(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/d/f": "x", "t/g": "y"})
	r.tree(r.dstDir, map[string]string{"t/d": "a file"})
	r.start()
	id := r.submit(cpSpec("move", "t", ""))
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.FailedFiles != 1 || j.Failures[0].Code != vfs.CodeConflict {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "t/d/f", "x")
	r.wantFile(r.dstDir, "t/d", "a file")
	r.wantFile(r.dstDir, "t/g", "y")
}

func TestDestinationParentSymlinkFailsItem(t *testing.T) {
	r := newRig(t)
	outside := t.TempDir()
	r.tree(r.srcDir, map[string]string{"t/d/f": "x"})
	if err := os.Mkdir(filepath.Join(r.dstDir, "t"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.dstDir, "t/d")); err != nil {
		t.Skip(err)
	}
	r.start()
	id := r.submit(cpSpec("move", "t", ""))
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeInvalidPath {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "t/d/f", "x")
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Error("wrote through the link")
	}
}

// lyingFS is a peer that returns hostile names from List.
type lyingFS struct {
	vfs.FS
	names []string
}

func (l lyingFS) List(ctx context.Context, p string) ([]vfs.Entry, error) {
	es, err := l.FS.List(ctx, p)
	if err != nil {
		return nil, err
	}
	if p == "t" {
		for _, n := range l.names {
			es = append(es, vfs.Entry{Name: n, Path: n, Type: vfs.TypeFile})
		}
	}
	return es, nil
}

func TestHostileListedNamesFailOnlyThatItem(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/ok": "ok"})
	r.srcF = lyingFS{r.src, []string{"../../evil", "a/b", "..", "."}}
	r.start()
	id := r.submit(cpSpec("copy", "t", "out"))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.FailedFiles != 4 || j.DoneFiles != 1 {
		t.Fatalf("%+v", j)
	}
	r.wantFile(r.dstDir, "out/ok", "ok")
	if r.exists(r.dstDir, "evil") || r.exists(r.dstDir, "out/a") || r.exists(filepath.Dir(r.dstDir), "evil") {
		t.Error("escaped")
	}
}

func TestDiskFullFailsJobThenRetry(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/a": "aa", "t/b": "bb", "t/c": "cc"})
	r.dstF = failDest{LocalFS: r.dst, commitErr: vfs.E(vfs.CodeDiskFull, "full")}
	r.start()
	id := r.submit(cpSpec("move", "t", ""))
	j := waitState(t, r.m, id, StateFailed, StatePartial, StateDone)
	if j.State != StateFailed || j.Error == nil || j.Error.Code != vfs.CodeDiskFull {
		t.Fatalf("%+v", j)
	}
	r.wantFile(r.srcDir, "t/a", "aa")
	if j.DoneFiles != 0 {
		t.Errorf("%+v", j)
	}

	r.set(func() { r.dstF = r.dst })
	if err := r.m.Retry(id); err != nil {
		t.Fatal(err)
	}
	j = r.waitDone(id)
	if j.Error != nil || j.DoneFiles != 3 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "t/c", "cc")
	r.wantGone(r.srcDir, "t")
}

func TestRetryAfterPartial(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/a": "aa", "t/b": "bb"})
	r.tree(r.dstDir, map[string]string{"t/a": "old"})
	r.start()
	id := r.submit(cpSpec("move", "t", ""))
	j := waitState(t, r.m, id, StatePartial)
	if j.DoneFiles != 1 || j.FailedFiles != 1 {
		t.Fatalf("%+v", j)
	}
	if err := os.Remove(filepath.Join(r.dstDir, "t/a")); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Retry(id); err != nil {
		t.Fatal(err)
	}
	j = r.waitDone(id)
	if j.DoneFiles != 2 || j.FailedFiles != 0 || len(j.Failures) != 0 {
		t.Errorf("%+v", j)
	}
	r.wantGone(r.srcDir, "t")
}

func TestRetryAndCancelStateRules(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "x"})
	r.start()
	id := r.submit(cpSpec("copy", "f", "f"))
	r.waitDone(id)
	wantCode(t, r.m.Retry(id), vfs.CodeInvalidRequest)
	wantCode(t, r.m.Cancel(id), vfs.CodeInvalidRequest)
	wantCode(t, r.m.Retry("nope"), vfs.CodeNotFound)
	wantCode(t, r.m.Cancel("nope"), vfs.CodeNotFound)
	_, err := r.m.Get("nope")
	wantCode(t, err, vfs.CodeNotFound)
}

func TestCancelRemovesParts(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/a": "aa"})
	big := randBytes(2 << 20)
	r.write(t, r.srcDir, "t/b", big)
	f := newFault(r.src)
	f.hookPath, f.hookAt = "t/b", 1<<20
	r.srcF = f
	r.start()
	var parts atomic.Int64
	var id atomic.Value
	release := make(chan struct{})
	f.hook = func() {
		ents, _ := os.ReadDir(filepath.Join(r.dstDir, vfs.PartDir))
		parts.Store(int64(len(ents)))
		<-release
		if err := r.m.Cancel(id.Load().(string)); err != nil {
			t.Error(err)
		}
	}
	id.Store(r.submit(cpSpec("move", "t", "")))
	close(release)
	j := waitState(t, r.m, id.Load().(string), StateCanceled, StateDone, StateFailed, StatePartial)
	if j.State != StateCanceled {
		t.Fatalf("%+v", j)
	}
	if parts.Load() == 0 {
		t.Error("no part during transfer: the test proved nothing")
	}
	r.noParts(t)
	r.wantFile(r.dstDir, "t/a", "aa") // already moved stays moved
	r.wantGone(r.srcDir, "t/a")
	r.wantGone(r.dstDir, "t/b")
	if r.read(t, r.srcDir, "t/b") != string(big) {
		t.Error("source changed")
	}
}

func TestCancelWhileWaitingRemovesParts(t *testing.T) {
	r := newRig(t)
	r.write(t, r.srcDir, "big", randBytes(1<<20))
	f := newFault(r.src)
	f.setDrop(256 << 10)
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("copy", "big", "big"))
	waitState(t, r.m, id, StateWaitingPeer)
	r.set(func() { r.down = true })
	waitState(t, r.m, id, StateWaitingPeer)
	if err := r.m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, r.m, id, StateCanceled)
	r.noParts(t)
	r.set(func() { r.down = false })
	time.Sleep(30 * time.Millisecond)
	if j, _ := r.m.Get(id); j.State != StateCanceled {
		t.Errorf("revived: %+v", j)
	}
}

func TestCancelQueuedNeverRuns(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "x"})
	m, err := NewManager(r.opts) // not running yet
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Submit("o", cpSpec("copy", "f", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if j.State != StateQueued {
		t.Errorf("state %s", j.State)
	}
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	r.start() // a new Manager over the same Dir: canceled is final
	if got, _ := r.m.Get(j.ID); got.State != StateCanceled {
		t.Errorf("state after restart: %+v", got)
	}
	time.Sleep(20 * time.Millisecond)
	r.wantGone(r.dstDir, "f")
}

func TestCancelSurvivesKill(t *testing.T) {
	r := newRig(t)
	r.write(t, r.srcDir, "big", randBytes(1<<20))
	f := newFault(r.src)
	f.setDrop(256 << 10)
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("copy", "big", "big"))
	waitState(t, r.m, id, StateWaitingPeer)
	// Simulate a kill after the cancel request was persisted, before it ran.
	r.stop()
	path := filepath.Join(r.dir, "jobs", id, "job.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	rec.CancelRequested = true
	b, _ = json.Marshal(rec)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	r.srcF = r.src
	r.start()
	if j := waitState(t, r.m, id, StateCanceled, StateDone); j.State != StateCanceled {
		t.Errorf("%+v", j)
	}
	r.noParts(t)
	r.wantGone(r.dstDir, "big")
}

// slowFS measures how many streams are open at once. Streams wait until a
// second one has arrived, so jobs that can overlap always do, however the
// scheduler happens to time them; only serial execution reaches the deadline.
type slowFS struct {
	vfs.FS
	mu       sync.Mutex
	cur, max int
	met      chan struct{}
	once     sync.Once
}

func (s *slowFS) Open(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error) {
	s.mu.Lock()
	s.cur++
	s.max = max(s.max, s.cur)
	if s.cur >= 2 {
		s.once.Do(func() { close(s.met) })
	}
	s.mu.Unlock()
	select {
	case <-s.met:
		time.Sleep(20 * time.Millisecond) // room for a third, if one is allowed
	case <-time.After(waitTime / 2):
	}
	s.mu.Lock()
	s.cur--
	s.mu.Unlock()
	return s.FS.Open(ctx, p, off, etag)
}

func TestConcurrencyLimit(t *testing.T) {
	r := newRig(t)
	files := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		files[n] = n
	}
	r.tree(r.srcDir, files)
	s := &slowFS{FS: r.src, met: make(chan struct{})}
	r.srcF = s
	r.start()
	var ids []string
	for n := range files {
		ids = append(ids, r.submit(cpSpec("copy", n, n)))
	}
	var peak int
	deadline := time.Now().Add(waitTime)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("jobs did not finish: %+v", r.m.List(""))
		}
		running, done := 0, 0
		for _, j := range r.m.List("") {
			switch j.State {
			case StateRunning:
				running++
			case StateDone:
				done++
			}
		}
		peak = max(peak, running)
		if done == len(ids) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if peak > 2 || s.max > 2 {
		t.Errorf("peak running %d, streams %d, limit 2", peak, s.max)
	}
	if s.max < 2 {
		t.Errorf("never ran in parallel: %d", s.max)
	}
}

func TestFailureListCapped(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Join(r.srcDir, "t"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.dstDir, "t"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 1500 {
		name := "f" + strconv.Itoa(i)
		r.write(t, r.srcDir, "t/"+name, []byte("n"))
		r.write(t, r.dstDir, "t/"+name, []byte("o"))
	}
	r.start()
	id := r.submit(cpSpec("copy", "t", ""))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || len(j.Failures) != 1000 || j.FailedFiles != 1500 {
		t.Errorf("state %s failures %d failed %d", j.State, len(j.Failures), j.FailedFiles)
	}
}

func TestCorruptJobFileSkipped(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "x"})
	m, err := NewManager(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Submit("o", cpSpec("copy", "f", "f"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"garbage":  "{not json",
		"empty":    "",
		"wrongid":  `{"ID":"../../elsewhere","State":"queued"}`,
		"nostate":  `{"ID":"nostate"}`,
		"badstate": `{"ID":"badstate","State":"who-knows"}`,
	} {
		p := filepath.Join(r.dir, "jobs", name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "job.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "jobs", "nojson"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.waitDone(j.ID)
	if js := r.m.List(""); len(js) != 1 {
		t.Errorf("jobs: %+v", js)
	}
}

func TestTornDoneLogLineIgnored(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"t/a": "aa"})
	r.write(t, r.srcDir, "t/b", randBytes(1<<20))
	f := newFault(r.src)
	f.hookPath, f.hookAt = "t/b", 512<<10
	r.srcF = f
	r.start()
	f.hook = func() { r.cancel() }
	id := r.submit(cpSpec("copy", "t", ""))
	r.waitStopped()
	lf, err := os.OpenFile(filepath.Join(r.dir, "jobs", id, "done.log"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	lf.WriteString(`{"p":"t/b","d":"t`) // a kill mid-append
	lf.Close()

	r.srcF = r.src
	r.start()
	j := r.waitDone(id)
	if j.DoneFiles != 2 {
		t.Errorf("%+v", j)
	}
	if !r.exists(r.dstDir, "t/b") {
		t.Error("torn entry counted as done")
	}
	// Everything written after the torn line must still parse.
	r.stop()
	done, err := readDoneLog(filepath.Join(r.dir, "jobs", id, "done.log"))
	if err != nil || len(done) != 2 {
		t.Errorf("done.log entries %v, %v", done, err)
	}
}

func TestJobFilePrivateAndComplete(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "x"})
	r.start()
	id := r.submit(cpSpec("copy", "f", "f"))
	r.waitDone(id)
	r.stop()
	m, err := NewManager(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != StateDone || j.DoneFiles != 1 || j.Created.IsZero() || j.Updated.IsZero() {
		t.Errorf("%+v", j)
	}
	if js := m.List("origin"); len(js) != 1 {
		t.Errorf("origin filter: %+v", js)
	}
	if js := m.List("other"); len(js) != 0 {
		t.Errorf("origin filter: %+v", js)
	}
	fi, err := os.Stat(filepath.Join(r.dir, "jobs", id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no such bits: it reports 0o666 for every writable file.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func TestSnapshotsDoNotAlias(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "new"})
	r.tree(r.dstDir, map[string]string{"f": "old"})
	r.start()
	id := r.submit(cpSpec("copy", "f", "f"))
	j := waitState(t, r.m, id, StatePartial)
	j.Failures[0].Code = "tampered"
	again, _ := r.m.Get(id)
	if again.Failures[0].Code == "tampered" {
		t.Error("Get exposes internal state")
	}
}

func openRO(dir string) (*shares.LocalFS, error) {
	return shares.Open(shares.Spec{Name: "ro", Path: dir, ReadOnly: true})
}

// foldDest behaves like a case-insensitive destination: names differing only
// in case are the same file.
type foldDest struct{ Dest }

func fold(p string) string { return strings.ToLower(p) }

func (d foldDest) Stat(ctx context.Context, p string) (vfs.Entry, error) {
	return d.Dest.Stat(ctx, fold(p))
}
func (d foldDest) List(ctx context.Context, p string) ([]vfs.Entry, error) {
	return d.Dest.List(ctx, fold(p))
}
func (d foldDest) Hash(ctx context.Context, p string) (string, error) {
	return d.Dest.Hash(ctx, fold(p))
}
func (d foldDest) Mkdir(ctx context.Context, p string) error {
	return d.Dest.Mkdir(ctx, fold(p))
}
func (d foldDest) Remove(ctx context.Context, p string, recursive bool) error {
	return d.Dest.Remove(ctx, fold(p), recursive)
}
func (d foldDest) Rename(ctx context.Context, from, to string) error {
	return d.Dest.Rename(ctx, fold(from), fold(to))
}
func (d foldDest) CommitPart(key, dst string, mt time.Time, mode uint32) error {
	return d.Dest.CommitPart(key, fold(dst), mt, mode)
}

func (d foldDest) SyncParent(p string) error { return d.Dest.SyncParent(fold(p)) }

func TestCaseOnlyCollisionKeepsSecondSource(t *testing.T) {
	for _, op := range []string{"move", "copy"} {
		for _, c := range []Conflict{ConflictFail, ConflictSkip, ConflictOverwrite, ConflictRename} {
			t.Run(op+"/"+string(c), func(t *testing.T) {
				r := newRig(t)
				r.named()
				r.opts.GOOS = "darwin"
				r.dstF = foldDest{r.dstView}
				r.tree(r.srcDir, map[string]string{"t/A": "upper", "t/a": "lower", "t/z": "other"})
				r.start()
				s := cpSpec(op, "t", "")
				s.OnConflict = c
				id := r.submit(s)
				check := func() {
					t.Helper()
					j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
					if j.State != StatePartial || j.DoneFiles != 2 || j.FailedFiles != 1 {
						t.Fatalf("%+v", j)
					}
					if f := j.Failures[0]; f.Code != vfs.CodeConflict || f.Path != "t/a" {
						t.Errorf("%+v", f)
					}
					r.wantFile(r.dstDir, "t/a", "upper")
					r.wantFile(r.dstDir, "t/z", "other")
					r.wantFile(r.srcDir, "t/a", "lower")
					if ents, _ := os.ReadDir(filepath.Join(r.dstDir, "t")); len(ents) != 2 {
						t.Errorf("destination: %v", ents)
					}
					if op == "move" {
						r.wantGone(r.srcDir, "t/A")
					} else {
						r.wantFile(r.srcDir, "t/A", "upper")
					}
				}
				check()
				// The taken name is remembered across a Retry through done.log.
				if err := r.m.Retry(id); err != nil {
					t.Fatal(err)
				}
				check()
			})
		}
	}
}

func TestCaseOnlyNamesBothArriveOnCaseSensitiveDest(t *testing.T) {
	r := newRig(t)
	r.named()
	r.tree(r.srcDir, map[string]string{"t/A": "upper", "t/a": "lower"})
	r.start()
	j := r.waitDone(r.submit(cpSpec("move", "t", "")))
	if j.DoneFiles != 2 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "t/A", "upper")
	r.wantFile(r.dstDir, "t/a", "lower")
}

// On a disk that really folds the names, with the real LocalFS and this OS's
// rules: nothing above emulates anything here. The source holds both names, as
// one on another host can. Linux has no such disk by default, and there the
// tests above show the logic through foldDest and apfsDest.
func TestCollisionOnARealFoldingDisk(t *testing.T) {
	for _, c := range collisionNames {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			if !diskFolds(t, r.dstDir, c.first, c.second) {
				t.Skipf("this disk keeps %q and %q apart", c.first, c.second)
			}
			r.opts.GOOS = runtime.GOOS
			r.srcF = namedFS{r.src}
			first, second := diskName("t/"+c.first), diskName("t/"+c.second)
			r.tree(r.srcDir, map[string]string{first: "first", second: "second"})
			r.start()
			s := cpSpec("move", "t", "")
			s.OnConflict = ConflictOverwrite
			id := r.submit(s)
			j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
			if j.State != StatePartial || j.DoneFiles != 1 || j.FailedFiles != 1 || j.Failures[0].Code != vfs.CodeConflict {
				t.Fatalf("%+v", j)
			}
			r.wantFile(r.dstDir, "t/"+c.first, "first")
			r.wantFile(r.srcDir, second, "second") // the only copy of it
			if r.env.exists(r.srcDir, first) {
				t.Errorf("%s was not moved", c.first)
			}
			if ents, _ := os.ReadDir(filepath.Join(r.dstDir, "t")); len(ents) != 1 {
				t.Errorf("destination: %v", ents)
			}
		})
	}
}

// And on a disk that keeps them apart, both really arrive under their own names.
func TestCollisionNamesOnARealPreservingDisk(t *testing.T) {
	for _, c := range collisionNames {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			if diskFolds(t, r.dstDir, c.first, c.second) {
				t.Skipf("this disk folds %q and %q into one file", c.first, c.second)
			}
			r.tree(r.srcDir, map[string]string{"t/" + c.first: "first", "t/" + c.second: "second"})
			r.start()
			j := r.waitDone(r.submit(cpSpec("move", "t", "")))
			if j.DoneFiles != 2 {
				t.Errorf("%+v", j)
			}
			r.wantFile(r.dstDir, "t/"+c.first, "first")
			r.wantFile(r.dstDir, "t/"+c.second, "second")
		})
	}
}

// diskFolds reports whether a file named first can be reached as second in dir.
func diskFolds(t *testing.T, dir, first, second string) bool {
	t.Helper()
	p := filepath.Join(dir, first)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	_, err := os.Lstat(filepath.Join(dir, second))
	return err == nil
}

// a/a into the root resolves to a: a/a/a/x would land on a/a/x, the job's own
// source of a later item.
func TestMoveIntoOwnParentKeepsOwnSources(t *testing.T) {
	for _, op := range []string{"move", "copy"} {
		for _, c := range []Conflict{ConflictOverwrite, ConflictFail, ConflictRename} {
			t.Run(op+"/"+string(c), func(t *testing.T) {
				r := newRig(t)
				r.tree(r.dstDir, map[string]string{"a/a/x": "ORIGINAL-x", "a/a/a/x": "nested-x"})
				r.start()
				id := r.submit(sameShare(op, "a/a", "", c))
				j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
				if j.State != StatePartial || j.FailedFiles != 1 || j.DoneFiles != 1 {
					t.Fatalf("%+v", j)
				}
				if f := j.Failures[0]; f.Code != vfs.CodeConflict || f.Path != "a/a/a/x" {
					t.Errorf("%+v", f)
				}
				r.wantFile(r.dstDir, "a/x", "ORIGINAL-x")
				r.wantFile(r.dstDir, "a/a/a/x", "nested-x")
				if op == "copy" {
					r.wantFile(r.dstDir, "a/a/x", "ORIGINAL-x")
				} else {
					r.wantGone(r.dstDir, "a/a/x")
				}
				r.wantGone(r.dstDir, "a/a/x (1)")
			})
		}
	}
}

// An empty directory that would land under the job's source fails too, and
// stays where it is.
func TestMoveIntoOwnParentKeepsEmptyDir(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"a/a/x": "x"})
	if err := os.MkdirAll(filepath.Join(r.dstDir, "a/a/a/e"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.start()
	id := r.submit(sameShare("move", "a/a", "", ConflictFail))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.FailedFiles != 1 || j.Failures[0].Code != vfs.CodeConflict {
		t.Fatalf("%+v", j)
	}
	r.wantFile(r.dstDir, "a/x", "x")
	if !r.exists(r.dstDir, "a/a/a/e") {
		t.Error("empty source directory removed")
	}
	r.wantGone(r.dstDir, "a/a/e")
}

// An item logged before the rule existed is finished, not failed.
func TestLoggedItemUnderSourceRootStaysDone(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"a/a/x": "x", "a/a/a/x": "n"})
	r.start()
	id := r.submit(sameShare("copy", "a/a", "", ConflictFail))
	waitState(t, r.m, id, StatePartial)
	lf, err := os.OpenFile(filepath.Join(r.dir, "jobs", id, "done.log"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	lf.WriteString(`{"p":"a/x","d":"a/a/x","s":1}` + "\n")
	lf.Close()
	if err := r.m.Retry(id); err != nil {
		t.Fatal(err)
	}
	j := r.waitDone(id)
	if j.DoneFiles != 2 || j.FailedFiles != 0 {
		t.Errorf("%+v", j)
	}
}

func TestActivePart(t *testing.T) {
	r := newRig(t)
	r.write(t, r.srcDir, "wait", randBytes(1<<20))
	r.tree(r.srcDir, map[string]string{"ok": "x", "clash": "new", "full": "y"})
	r.tree(r.dstDir, map[string]string{"clash": "old"})
	f := newFault(r.src)
	r.srcF = f
	r.start()
	if r.m.ActivePart("unknown") {
		t.Error("unknown key is active")
	}

	done := r.submit(cpSpec("copy", "ok", "ok"))
	r.waitDone(done)
	if r.m.ActivePart(partKey(done, "")) {
		t.Error("done job's part is active")
	}

	// A partial job's parts were removed and a Retry starts over.
	f.setDrop(0)
	f.dropCode = vfs.CodeInternal
	partial := r.submit(cpSpec("copy", "full", "elsewhere"))
	waitState(t, r.m, partial, StatePartial)
	if r.m.ActivePart(partKey(partial, "")) {
		t.Error("partial job's part is active")
	}
	f.setDrop(-1)
	f.dropCode = ""

	r.set(func() { r.dstF = failDest{LocalFS: r.dst, commitErr: vfs.E(vfs.CodeDiskFull, "full")} })
	failed := r.submit(cpSpec("copy", "full", "full"))
	waitState(t, r.m, failed, StateFailed)
	if !r.m.ActivePart(partKey(failed, "")) {
		t.Error("failed job's part must survive for Retry")
	}
	r.set(func() { r.dstF = r.dst })

	f.setDrop(256 << 10)
	waiting := r.submit(cpSpec("copy", "wait", "wait"))
	waitState(t, r.m, waiting, StateWaitingPeer)
	r.set(func() { r.down = true })
	if !r.m.ActivePart(partKey(waiting, "")) {
		t.Error("waiting job's part is active")
	}

	// A restart must still know the keys, before any attempt has run.
	r.stop()
	m, err := NewManager(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !m.ActivePart(partKey(waiting, "")) || !m.ActivePart(partKey(failed, "")) {
		t.Error("keys lost across a restart")
	}
	if m.ActivePart(partKey(done, "")) || m.ActivePart(partKey(partial, "")) {
		t.Error("finished jobs' keys active after a restart")
	}

	if err := m.Cancel(waiting); err != nil {
		t.Fatal(err)
	}
	if m.ActivePart(partKey(waiting, "")) {
		t.Error("canceled job's part is active")
	}
	if err := m.Cancel(failed); err == nil {
		t.Error("failed job is not cancelable")
	}
}

// apfsDest behaves like APFS: names differing by case or by Unicode
// normalization are one file. It records the names the job asked for.
type apfsDest struct {
	Dest
	mu        sync.Mutex
	committed []string
}

func apfsName(p string) string { return norm.NFC.String(strings.ToLower(p)) }

func (d *apfsDest) Stat(ctx context.Context, p string) (vfs.Entry, error) {
	return d.Dest.Stat(ctx, apfsName(p))
}
func (d *apfsDest) List(ctx context.Context, p string) ([]vfs.Entry, error) {
	return d.Dest.List(ctx, apfsName(p))
}
func (d *apfsDest) Hash(ctx context.Context, p string) (string, error) {
	return d.Dest.Hash(ctx, apfsName(p))
}
func (d *apfsDest) Mkdir(ctx context.Context, p string) error {
	return d.Dest.Mkdir(ctx, apfsName(p))
}
func (d *apfsDest) Remove(ctx context.Context, p string, recursive bool) error {
	return d.Dest.Remove(ctx, apfsName(p), recursive)
}
func (d *apfsDest) Rename(ctx context.Context, from, to string) error {
	return d.Dest.Rename(ctx, apfsName(from), apfsName(to))
}
func (d *apfsDest) CommitPart(key, dst string, mt time.Time, mode uint32) error {
	d.mu.Lock()
	d.committed = append(d.committed, dst)
	d.mu.Unlock()
	return d.Dest.CommitPart(key, apfsName(dst), mt, mode)
}

func (d *apfsDest) SyncParent(p string) error { return d.Dest.SyncParent(apfsName(p)) }

var (
	koreanNFC = norm.NFC.String("한글.txt")
	koreanNFD = norm.NFD.String("한글.txt")
)

func TestNormalizationCollisionKeepsSecondSource(t *testing.T) {
	if koreanNFC == koreanNFD {
		t.Fatal("the two spellings must differ")
	}
	for _, op := range []string{"move", "copy"} {
		t.Run(op, func(t *testing.T) {
			r := newRig(t)
			r.named()
			r.opts.GOOS = "darwin"
			d := &apfsDest{Dest: r.dstView}
			r.dstF = d
			// The walk is sorted by bytes: the NFD spelling comes first.
			r.tree(r.srcDir, map[string]string{"t/" + koreanNFD: "nfd-content", "t/" + koreanNFC: "nfc-content"})
			r.start()
			s := cpSpec(op, "t", "")
			s.OnConflict = ConflictOverwrite
			id := r.submit(s)
			j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
			if j.State != StatePartial || j.DoneFiles != 1 || j.FailedFiles != 1 {
				t.Fatalf("%+v", j)
			}
			if f := j.Failures[0]; f.Code != vfs.CodeConflict || f.Path != "t/"+koreanNFC {
				t.Errorf("%+v", f)
			}
			r.wantFile(r.dstDir, "t/"+koreanNFC, "nfd-content") // apfsDest stores NFC
			if ents, _ := os.ReadDir(filepath.Join(r.dstDir, "t")); len(ents) != 1 {
				t.Errorf("destination: %v", ents)
			}
			r.wantFile(r.srcDir, "t/"+koreanNFC, "nfc-content")
			if op == "move" {
				r.wantGone(r.srcDir, "t/"+koreanNFD)
			} else {
				r.wantFile(r.srcDir, "t/"+koreanNFD, "nfd-content")
			}
			// The key is for comparison only: the job asked for the bytes it read.
			d.mu.Lock()
			defer d.mu.Unlock()
			if !slices.Equal(d.committed, []string{"t/" + koreanNFD}) {
				t.Errorf("committed %q", d.committed)
			}
		})
	}
}

// Windows folds case but does not normalize: the two spellings are two files.
// A destination that keeps bytes keeps them exactly, on any OS.
func TestNormalizationSpellingsKeptByteForByte(t *testing.T) {
	r := newRig(t)
	r.named()
	r.opts.GOOS = "windows"
	r.tree(r.srcDir, map[string]string{"t/" + koreanNFD: "nfd-content", "t/" + koreanNFC: "nfc-content"})
	r.start()
	j := r.waitDone(r.submit(cpSpec("move", "t", "")))
	if j.DoneFiles != 2 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "t/"+koreanNFD, "nfd-content")
	r.wantFile(r.dstDir, "t/"+koreanNFC, "nfc-content")

	r2 := newRig(t)
	r2.named()
	r2.opts.GOOS = "darwin"
	r2.tree(r2.srcDir, map[string]string{koreanNFD: "only"})
	r2.start()
	r2.waitDone(r2.submit(cpSpec("copy", koreanNFD, "")))
	r2.wantFile(r2.dstDir, koreanNFD, "only")
	r2.wantGone(r2.dstDir, koreanNFC)
}

// The self-move check uses the same key: the NFC spelling of a directory is
// the NFD directory on darwin.
func TestSelfMoveNormalizedSpellingRejected(t *testing.T) {
	nfcDir, nfdDir := norm.NFC.String("폴더"), norm.NFD.String("폴더")
	r := newRig(t)
	r.opts.GOOS = "darwin"
	r.tree(r.dstDir, map[string]string{nfdDir + "/x": "1"})
	r.start()
	s := Spec{Op: "move", Src: Endpoint{Peer: self, Share: "d", Path: nfdDir}, Dst: Endpoint{Peer: self, Share: "d", Path: nfcDir + "/b"}}
	_, err := r.m.Submit("o", s)
	wantCode(t, err, vfs.CodeInvalidRequest)
}

// powerLoss models a power cut: the only log bytes that survive are those
// covered by a completed sync, while source removals are already durable.
type powerLoss struct {
	mu    sync.Mutex
	sizes map[string]int64
	syncs int
}

func (r *rig) trackSyncs() *powerLoss {
	p := &powerLoss{sizes: map[string]int64{}}
	r.opts.syncFile = func(f *os.File) error {
		if err := f.Sync(); err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			return err
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.syncs++
		p.sizes[f.Name()] = st.Size()
		return nil
	}
	return p
}

// cut drops everything in the job's logs that was never synced.
func (p *powerLoss) cut(t *testing.T, jobDir string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range []string{"done.log", "renames.log"} {
		path := filepath.Join(jobDir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Truncate(path, p.sizes[path]); err != nil {
			t.Fatal(err)
		}
	}
}

var collisionNames = []struct{ name, first, second string }{
	{"case", "A", "a"},
	{"normalization", koreanNFD, koreanNFC},
}

// A power cut right after the first source was removed. If the log entry were
// not durable by then, nothing would say the destination is this job's file
// and the second item would overwrite the only copy.
func TestPowerCutAfterSourceRemovalLosesNothing(t *testing.T) {
	for _, c := range collisionNames {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.named()
			r.opts.GOOS = "darwin"
			power := r.trackSyncs()
			r.dstF = &apfsDest{Dest: r.dstView}
			r.tree(r.srcDir, map[string]string{"t/" + c.first: "first", "t/" + c.second: "second"})
			f := newFault(r.srcView)
			var once sync.Once
			f.onRemove = func(p string) error {
				var err error
				once.Do(func() {
					if rerr := os.Remove(filepath.Join(r.srcDir, r.onDisk(p))); rerr != nil {
						t.Error(rerr)
					}
					r.cancel()
					err = vfs.E(vfs.CodeCanceled, "power cut")
				})
				return err
			}
			r.srcF = f
			r.start()
			s := cpSpec("move", "t", "")
			s.OnConflict = ConflictOverwrite
			id := r.submit(s)
			r.waitStopped()
			r.wantGone(r.srcDir, "t/"+c.first)
			power.cut(t, filepath.Join(r.dir, "jobs", id))

			r.srcF = r.srcView
			r.start()
			j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
			if j.State != StatePartial || j.FailedFiles != 1 || j.Failures[0].Code != vfs.CodeConflict {
				t.Errorf("%+v", j)
			}
			r.wantFile(r.dstDir, "t/"+apfsName(c.first), "first")
			r.wantFile(r.srcDir, "t/"+c.second, "second")
		})
	}
}

// renameCollisionDest is a same-share destination on a folding filesystem
// whose source names were written elsewhere, so both spellings exist. It can
// die right after a rename.
type renameCollisionDest struct {
	*apfsDest
	once sync.Once
	kill func()
	err  error // if set, every Rename fails with it
}

func (d *renameCollisionDest) Rename(ctx context.Context, from, to string) error {
	if d.err != nil {
		return d.err
	}
	if err := d.Dest.Rename(ctx, from, apfsName(to)); err != nil {
		return err
	}
	var ret error
	d.once.Do(func() {
		if d.kill != nil {
			d.kill()
			ret = vfs.E(vfs.CodeCanceled, "killed")
		}
	})
	return ret
}

// Killed between a same-share rename and its log entry: the source is gone
// and done.log knows nothing. The durable rename record must say whose file
// the destination is, after a plain kill and after a power cut.
func TestKillAfterItemRenameLosesNothing(t *testing.T) {
	for _, c := range collisionNames {
		for _, cut := range []bool{false, true} {
			t.Run(c.name+"/powercut="+strconv.FormatBool(cut), func(t *testing.T) {
				r := newRig(t)
				r.named()
				r.opts.GOOS = "darwin"
				power := r.trackSyncs()
				r.tree(r.dstDir, map[string]string{"t/" + c.first: "first", "t/" + c.second: "second"})
				if err := os.MkdirAll(filepath.Join(r.dstDir, "u/t"), 0o755); err != nil {
					t.Fatal(err)
				}
				r.selfSrc = r.dstView
				r.dstF = &renameCollisionDest{apfsDest: &apfsDest{Dest: r.dstView}, kill: func() { r.cancel() }}
				r.start()
				id := r.submit(sameShare("move", "t", "u", ConflictOverwrite))
				r.waitStopped()
				r.wantGone(r.dstDir, "t/"+c.first)
				if cut {
					power.cut(t, filepath.Join(r.dir, "jobs", id))
				}

				r.dstF = &renameCollisionDest{apfsDest: &apfsDest{Dest: r.dstView}}
				r.start()
				j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
				if j.State != StatePartial || j.DoneFiles != 1 || j.FailedFiles != 1 || j.Failures[0].Code != vfs.CodeConflict {
					t.Errorf("%+v", j)
				}
				r.wantFile(r.dstDir, "u/t/"+apfsName(c.first), "first")
				r.wantFile(r.dstDir, "t/"+c.second, "second")
			})
		}
	}
}

// The whole source vanished item by item and the logs hold only rename
// records: the job finishes rather than failing not_found.
func TestAllItemsRenamedThenKilledFinishes(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"t/a": "aa"})
	if err := os.MkdirAll(filepath.Join(r.dstDir, "u/t"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.dstF = &renameKillDest{Dest: r.dst, kill: func() { r.cancel() }}
	r.start()
	id := r.submit(sameShare("move", "t", "u", ConflictFail))
	r.waitStopped()
	if err := os.Remove(filepath.Join(r.dstDir, "t")); err != nil {
		t.Fatal(err)
	}
	r.dstF = r.dst
	r.start()
	j := r.waitDone(id)
	if j.DoneFiles != 1 || j.DoneBytes != 2 || j.TotalFiles != 1 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "u/t/a", "aa")
}

// A rename that failed left its record behind, and the source is still there:
// the record means nothing and the item is processed normally.
func TestRenameRecordWithSourcePresentIsIgnored(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"t/a": "aa", "u/t/a": "old"})
	r.start()
	id := r.submit(sameShare("move", "t", "u", ConflictFail))
	j := waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.DoneFiles != 0 || j.Failures[0].Code != vfs.CodeConflict {
		t.Fatalf("%+v", j)
	}
	if err := r.m.Retry(id); err != nil {
		t.Fatal(err)
	}
	j = waitState(t, r.m, id, StatePartial, StateDone)
	if j.State != StatePartial || j.DoneFiles != 0 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "t/a", "aa")
	r.wantFile(r.dstDir, "u/t/a", "old")
}

// If the entry cannot be made durable the source must not go away, and a Retry
// finishes the item.
func TestLogSyncFailureKeepsSource(t *testing.T) {
	r := newRig(t)
	var broken atomic.Bool
	broken.Store(true)
	r.opts.syncFile = func(f *os.File) error {
		if broken.Load() && filepath.Base(f.Name()) == "done.log" {
			return vfs.E(vfs.CodeInternal, "sync failed")
		}
		return f.Sync()
	}
	r.tree(r.srcDir, map[string]string{"f": "data"})
	f := newFault(r.src)
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.DoneFiles != 0 || j.Failures[0].Code != vfs.CodeInternal {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "data")
	if n := countOf(f.callLog(), "remove f"); n != 0 {
		t.Error("source removal attempted")
	}
	broken.Store(false)
	if err := r.m.Retry(id); err != nil {
		t.Fatal(err)
	}
	r.waitDone(id)
	r.wantGone(r.srcDir, "f")
	r.wantFile(r.dstDir, "f", "data")
}

func TestRenameRecordSyncFailureKeepsSource(t *testing.T) {
	r := newRig(t)
	r.opts.syncFile = func(f *os.File) error {
		if filepath.Base(f.Name()) == "renames.log" {
			return vfs.E(vfs.CodeInternal, "sync failed")
		}
		return f.Sync()
	}
	r.tree(r.dstDir, map[string]string{"t/a": "aa"})
	if err := os.MkdirAll(filepath.Join(r.dstDir, "u/t"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFault(r.dst)
	r.dstF = faultDest{r.dst, f}
	r.start()
	id := r.submit(sameShare("move", "t", "u", ConflictFail))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.DoneFiles != 0 || j.FailedFiles != 1 || j.Failures[0].Code != vfs.CodeInternal {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "t/a", "aa")
	r.wantGone(r.dstDir, "u/t/a")
	if n := countOf(f.callLog(), "rename t/a"); n != 0 {
		t.Error("rename attempted without a durable record")
	}
}

// A rename that fails for any reason but a taken name fails that item once.
func TestItemRenameErrorFailsItemOnly(t *testing.T) {
	r := newRig(t)
	r.tree(r.dstDir, map[string]string{"t/a": "aa"})
	if err := os.MkdirAll(filepath.Join(r.dstDir, "u/t"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.selfSrc = r.dst
	r.dstF = &renameCollisionDest{apfsDest: &apfsDest{Dest: r.dst}, err: vfs.E(vfs.CodeInternal, "no")}
	r.start()
	id := r.submit(sameShare("move", "t", "u", ConflictFail))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.FailedFiles != 1 || j.DoneFiles != 0 {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.dstDir, "t/a", "aa")
	r.wantGone(r.dstDir, "u/t/a")
}

// A copy keeps its source, so a lost entry is found again by the crash rule
// and the job spends no sync on it.
func TestCopyDoesNotSyncLog(t *testing.T) {
	r := newRig(t)
	power := r.trackSyncs()
	r.tree(r.srcDir, map[string]string{"f": "x"})
	r.start()
	r.waitDone(r.submit(cpSpec("copy", "f", "f")))
	power.mu.Lock()
	defer power.mu.Unlock()
	if power.syncs != 0 {
		t.Errorf("%d syncs", power.syncs)
	}
}

// Every NFC and NFD spelling of a code point must share a key on darwin,
// including those whose case mapping differs between the two forms.
func TestDestKeyJoinsNormalizationForms(t *testing.T) {
	darwin := &Manager{o: Options{GOOS: "darwin"}}
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		s := string(r)
		nfc, nfd := norm.NFC.String(s), norm.NFD.String(s)
		if darwin.destKey(nfc) != darwin.destKey(nfd) {
			t.Errorf("U+%04X: %q != %q", r, darwin.destKey(nfc), darwin.destKey(nfd))
		}
	}
	if darwin.destKey("A") != darwin.destKey("a") {
		t.Error("case not folded")
	}
	windows := &Manager{o: Options{GOOS: "windows"}}
	if windows.destKey(koreanNFC) == windows.destKey(koreanNFD) {
		t.Error("windows does not normalize")
	}
	if windows.destKey("A") != windows.destKey("a") {
		t.Error("case not folded on windows")
	}
	linux := &Manager{o: Options{GOOS: "linux"}}
	if linux.destKey("A") == linux.destKey("a") {
		t.Error("linux keeps bytes")
	}
}

// syncFailDest is a destination whose directory syncs fail while broken is set.
type syncFailDest struct {
	Dest
	broken atomic.Bool
	mu     sync.Mutex
	synced []string
}

func (d *syncFailDest) SyncParent(p string) error {
	d.mu.Lock()
	d.synced = append(d.synced, p)
	d.mu.Unlock()
	if d.broken.Load() {
		return vfs.E(vfs.CodeInternal, "sync failed")
	}
	return d.Dest.SyncParent(p)
}

// CommitPart's directory sync failed, so the file is at the destination but not
// known durable. A move keeps its source, and a later attempt that finds the
// file must make it durable before it may delete the source.
func TestUnsyncedCommitKeepsSourceUntilDurable(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "data"})
	d := &syncFailDest{Dest: &hookDest{Dest: r.dst, err: vfs.E(vfs.CodeInternal, "dir sync failed"), afterCommit: func() {}}}
	d.broken.Store(true)
	r.dstF = d
	f := newFault(r.src)
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.DoneFiles != 0 || j.Failures[0].Code != vfs.CodeInternal {
		t.Fatalf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "data")
	r.wantFile(r.dstDir, "f", "data")

	if err := r.m.Retry(id); err != nil {
		t.Fatal(err)
	}
	j = waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
	if j.State != StatePartial || j.Failures[0].Code != vfs.CodeInternal {
		t.Fatalf("still unsynced: %+v", j)
	}
	r.wantFile(r.srcDir, "f", "data")
	if n := countOf(f.callLog(), "remove f"); n != 0 {
		t.Error("source removal attempted on an unsynced file")
	}

	d.broken.Store(false)
	if err := r.m.Retry(id); err != nil {
		t.Fatal(err)
	}
	r.waitDone(id)
	r.wantGone(r.srcDir, "f")
	r.wantFile(r.dstDir, "f", "data")
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.synced) != 2 || d.synced[1] != "f" {
		t.Errorf("synced %q", d.synced)
	}
}

// Killed between writing a done.log entry and syncing it: the entry is visible
// but not durable. The restart must make it durable before trusting it, or a
// power cut after the source removal leaves nothing to say whose file it is.
func TestKilledBeforeLogSyncThenPowerCut(t *testing.T) {
	for _, c := range collisionNames {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.named()
			r.opts.GOOS = "darwin"
			r.dstF = &apfsDest{Dest: r.dstView}
			r.tree(r.srcDir, map[string]string{"t/" + c.first: "first", "t/" + c.second: "second"})
			s := cpSpec("move", "t", "")
			s.OnConflict = ConflictOverwrite

			r.opts.syncFile = func(f *os.File) error {
				if filepath.Base(f.Name()) == "done.log" {
					r.cancel()
					return vfs.E(vfs.CodeCanceled, "killed")
				}
				return f.Sync()
			}
			r.start()
			id := r.submit(s)
			r.waitStopped()
			if r.doneLogLines(id) != 1 {
				t.Fatalf("done.log: %d entries", r.doneLogLines(id))
			}
			r.wantFile(r.srcDir, "t/"+c.first, "first")

			// The restart removes the first source; the power then goes.
			power := r.trackSyncs()
			logPath := filepath.Join(r.dir, "jobs", id, "done.log")
			f := newFault(r.srcView)
			var once sync.Once
			f.onRemove = func(p string) error {
				var err error
				once.Do(func() {
					st, serr := os.Stat(logPath)
					if serr != nil {
						t.Error(serr)
					}
					power.mu.Lock()
					covered := power.sizes[logPath]
					power.mu.Unlock()
					if covered != st.Size() {
						t.Errorf("source goes with %d of %d log bytes synced", covered, st.Size())
					}
					if rerr := os.Remove(filepath.Join(r.srcDir, r.onDisk(p))); rerr != nil {
						t.Error(rerr)
					}
					r.cancel()
					err = vfs.E(vfs.CodeCanceled, "power cut")
				})
				return err
			}
			r.srcF = f
			r.start()
			r.waitStopped()
			r.wantGone(r.srcDir, "t/"+c.first)
			power.cut(t, filepath.Join(r.dir, "jobs", id))

			r.srcF = r.srcView
			r.start()
			j := waitState(t, r.m, id, StatePartial, StateDone, StateFailed)
			if j.State != StatePartial || j.FailedFiles != 1 || j.Failures[0].Code != vfs.CodeConflict {
				t.Errorf("%+v", j)
			}
			r.wantFile(r.dstDir, "t/"+apfsName(c.first), "first")
			r.wantFile(r.srcDir, "t/"+c.second, "second")
		})
	}
}

// With done.log sync always failing, no number of Retries may remove the
// source: a visible entry is not a durable one.
func TestLogSyncAlwaysFailingNeverRemovesSource(t *testing.T) {
	r := newRig(t)
	var broken atomic.Bool
	broken.Store(true)
	r.opts.syncFile = func(f *os.File) error {
		if broken.Load() && filepath.Base(f.Name()) == "done.log" {
			return vfs.E(vfs.CodeInternal, "sync failed")
		}
		return f.Sync()
	}
	r.tree(r.srcDir, map[string]string{"f": "data"})
	f := newFault(r.src)
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	waitState(t, r.m, id, StatePartial)
	for range 2 {
		if err := r.m.Retry(id); err != nil {
			t.Fatal(err)
		}
		j := waitState(t, r.m, id, StateFailed, StatePartial, StateDone)
		if j.State != StateFailed || j.Error == nil || j.Error.Code != vfs.CodeInternal {
			t.Errorf("%+v", j)
		}
		r.wantFile(r.srcDir, "f", "data")
		if n := countOf(f.callLog(), "remove f"); n != 0 {
			t.Fatal("source removal attempted on an unsynced entry")
		}
	}
	broken.Store(false)
	if err := r.m.Retry(id); err != nil {
		t.Fatal(err)
	}
	r.waitDone(id)
	r.wantGone(r.srcDir, "f")
	r.wantFile(r.dstDir, "f", "data")
}

// done.log's own name must be durable before a source may go.
func TestJobDirSyncFailureKeepsSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		// syncDir does nothing there, so no directory sync can fail. A name's
		// durability is left to NTFS; TestLogSyncAlwaysFailingNeverRemovesSource
		// still covers the log's contents.
		t.Skip("Windows cannot fsync a directory")
	}
	r := newRig(t)
	r.opts.syncFile = func(f *os.File) error {
		if st, err := f.Stat(); err == nil && st.IsDir() {
			return &os.PathError{Op: "sync", Path: f.Name(), Err: syscall.EIO}
		}
		return f.Sync()
	}
	r.tree(r.srcDir, map[string]string{"f": "data"})
	f := newFault(r.src)
	r.srcF = f
	r.start()
	id := r.submit(cpSpec("move", "f", "f"))
	j := waitState(t, r.m, id, StateFailed, StatePartial, StateDone)
	if j.State != StateFailed || j.Error == nil || j.Error.Code != vfs.CodeInternal {
		t.Errorf("%+v", j)
	}
	r.wantFile(r.srcDir, "f", "data")
	if n := countOf(f.callLog(), "remove f"); n != 0 {
		t.Error("source removal attempted")
	}
}

func TestJobDirSyncUnsupportedIsNoOp(t *testing.T) {
	for _, unsupported := range []error{syscall.EINVAL, syscall.ENOTSUP} {
		r := newRig(t)
		calls := atomic.Int64{}
		r.opts.syncFile = func(f *os.File) error {
			if st, err := f.Stat(); err == nil && st.IsDir() {
				calls.Add(1)
				return &os.PathError{Op: "sync", Path: f.Name(), Err: unsupported}
			}
			return f.Sync()
		}
		r.tree(r.srcDir, map[string]string{"f": "data"})
		r.start()
		r.waitDone(r.submit(cpSpec("move", "f", "f")))
		r.wantGone(r.srcDir, "f")
		if runtime.GOOS != "windows" && calls.Load() == 0 {
			t.Errorf("%v: directory sync never tried", unsupported)
		}
		r.stop()
	}
}

// ---- removal of a host (spec 7) ----

func TestPeerRemovedStopsWaitingJob(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "data"})
	m := r.start()
	r.set(func() { r.down = true })
	id := r.submit(cpSpec("move", "f", "f"))
	waitState(t, m, id, StateWaitingPeer)
	m.PeerRemoved(peerB)
	j, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != StateFailed || j.Error == nil || j.Error.Code != vfs.CodePeerNotFound {
		t.Fatalf("job = %s %+v", j.State, j.Error)
	}
	r.set(func() { r.down = false })
	time.Sleep(50 * time.Millisecond) // a retry timer must not bring it back
	if j, _ := m.Get(id); j.State != StateFailed {
		t.Fatalf("job came back as %s", j.State)
	}
	r.wantFile(r.srcDir, "f", "data")
	if err := m.Retry(id); err != nil {
		t.Fatal(err)
	}
	if j := r.waitDone(id); j.State != StateDone {
		t.Fatalf("after retry: %s", j.State)
	}
	r.wantFile(r.dstDir, "f", "data")
}

func TestPeerRemovedIgnoresOtherHostsAndFinishedJobs(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "data"})
	m := r.start()
	id := r.submit(cpSpec("copy", "f", "f"))
	r.waitDone(id)
	m.PeerRemoved(peerB)
	m.PeerRemoved("someone-else")
	m.PeerRemoved(self)
	m.PeerRemoved("")
	if j, _ := m.Get(id); j.State != StateDone {
		t.Fatalf("finished job became %s", j.State)
	}
}

func TestRetryRefusedWhileOriginIsRemoved(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "data"})
	var removed atomic.Bool
	removed.Store(true)
	r.opts.Removed = func(id string) bool { return removed.Load() && id == "origin" }
	m := r.start()
	r.set(func() { r.down = true })
	j, err := m.Submit("origin", cpSpec("copy", "f", "f"))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, j.ID, StateWaitingPeer)
	m.PeerRemoved("origin")
	waitState(t, m, j.ID, StateCanceled)
	if err := m.Retry(j.ID); vfs.CodeOf(err) != vfs.CodePeerNotFound {
		t.Fatalf("Retry = %v, want peer_not_found", err)
	}
	r.set(func() { r.down = false })
	removed.Store(false)
	if err := m.Retry(j.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, j.ID, StateDone)
}

// A job from a removed source that was only queued at the kill: recovery fails
// it without ever contacting anything.
func TestRecoveryAppliesRemoval(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"f": "data", "g": "data"})
	m := r.start()
	r.set(func() { r.down = true })
	local, err := m.Submit(self, cpSpec("move", "f", "f"))
	if err != nil {
		t.Fatal(err)
	}
	fromB := local.ID
	byOrigin, err := m.Submit("origin", cpSpec("move", "g", "g"))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, fromB, StateWaitingPeer)
	waitState(t, m, byOrigin.ID, StateWaitingPeer)
	r.stop()
	r.opts.Removed = func(id string) bool { return id == peerB || id == "origin" }
	m, err = NewManager(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := m.Get(fromB); j.State != StateFailed || j.Error == nil || j.Error.Code != vfs.CodePeerNotFound {
		t.Fatalf("source removed: %s %+v", j.State, j.Error)
	}
	if j, _ := m.Get(byOrigin.ID); j.State != StateCanceled { // its source too: canceled wins
		t.Fatalf("origin removed: %s", j.State)
	}
	r.wantFile(r.srcDir, "f", "data")
	r.wantFile(r.srcDir, "g", "data")
}

// Spec 5 and 7: a symlink is recorded as skipped_symlink. It is not a failure.
func TestSkippedSymlinkRecorded(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"tree/f": "data"})
	if err := os.Symlink("f", filepath.Join(r.srcDir, "tree", "link")); err != nil {
		t.Skip(err)
	}
	r.start()
	id := r.submit(cpSpec("copy", "tree", "out"))
	j := r.waitDone(id)
	if j.State != StateDone || j.FailedFiles != 0 || j.SkippedFiles != 1 || len(j.Failures) != 0 {
		t.Fatalf("job = %+v", j)
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), SkippedSymlink) || !strings.Contains(string(b), "tree/link") {
		t.Fatalf("the job does not say what it skipped: %s", b)
	}
}

func TestSkippedSymlinkFields(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"tree/f": "data"})
	if err := os.Symlink("f", filepath.Join(r.srcDir, "tree", "link")); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink("tree", filepath.Join(r.srcDir, "top")); err != nil {
		t.Fatal(err)
	}
	r.start()
	j := r.waitDone(r.submit(cpSpec("copy", "tree", "out")))
	want := []ItemError{{"tree/link", SkippedSymlink, "symbolic links are not transferred"}}
	if !slices.Equal(j.Skipped, want) {
		t.Errorf("Skipped = %+v", j.Skipped)
	}
	j = r.waitDone(r.submit(cpSpec("copy", "top", "top")))
	if j.State != StateDone || len(j.Skipped) != 1 || j.Skipped[0].Path != "top" {
		t.Errorf("a link as the job's root: %+v", j)
	}
}

func TestSkippedSymlinksCapped(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"tree/f": "data"})
	const n = maxFailures + 5
	for i := range n {
		if err := os.Symlink("f", filepath.Join(r.srcDir, "tree", "l"+strconv.Itoa(i))); err != nil {
			t.Skip(err)
		}
	}
	r.start()
	j := r.waitDone(r.submit(cpSpec("copy", "tree", "out")))
	if j.State != StateDone || j.SkippedFiles != n || j.FailedFiles != 0 || len(j.Skipped) != maxFailures {
		t.Errorf("state %s, skipped %d, failed %d, listed %d", j.State, j.SkippedFiles, j.FailedFiles, len(j.Skipped))
	}
}

// A retry starts over, so it must not list what it skipped twice.
func TestSkippedSymlinksNotDoubledByRetry(t *testing.T) {
	r := newRig(t)
	r.tree(r.srcDir, map[string]string{"tree/f": "data"})
	r.tree(r.dstDir, map[string]string{"out/tree/f": "in the way"})
	if err := os.Symlink("f", filepath.Join(r.srcDir, "tree", "link")); err != nil {
		t.Skip(err)
	}
	m := r.start()
	first := r.submit(cpSpec("copy", "tree", "out"))
	if j := waitState(t, m, first, StatePartial); len(j.Skipped) != 1 {
		t.Fatalf("Skipped = %+v", j.Skipped)
	}
	if err := m.Retry(first); err != nil {
		t.Fatal(err)
	}
	if j := waitState(t, m, first, StatePartial); len(j.Skipped) != 1 {
		t.Fatalf("after a retry: Skipped = %+v", j.Skipped)
	}
}
