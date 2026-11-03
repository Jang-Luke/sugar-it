package davfs

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/vfs"
)

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

func gone(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("%s still exists (%v)", name, err)
	}
}

func dropStream(rc io.ReadCloser) io.ReadCloser {
	return &failAfter{rc: rc, left: 3, err: vfs.E(vfs.CodePeerOffline, "the peer went away")}
}

// failAfter gives a few bytes of the real stream, then fails like a dropped peer.
type failAfter struct {
	rc   io.ReadCloser
	left int
	err  error
}

func (f *failAfter) Read(b []byte) (int, error) {
	if f.left == 0 {
		return 0, f.err
	}
	n, err := f.rc.Read(b[:min(len(b), f.left)])
	f.left -= n
	return n, err
}
func (f *failAfter) Close() error { return f.rc.Close() }

// Each case runs on this host's share and on a peer's, where a recursive
// remove would go over the wire.
func bothSides(t *testing.T, f func(t *testing.T, e *env, share *fakeShare, root string)) {
	t.Run("local", func(t *testing.T) {
		e := setup(t)
		f(t, e, e.docs, "/local/docs/")
	})
	t.Run("peer", func(t *testing.T) {
		e := setup(t)
		write(t, e.pics.dir, "a.txt", "0123456789")
		f(t, e, e.pics, "/bob/pics/")
	})
}

func TestMoveMissingSourceKeepsDestination(t *testing.T) {
	bothSides(t, func(t *testing.T, e *env, s *fakeShare, root string) {
		os.MkdirAll(filepath.Join(s.dir, "keep", "sub"), 0o755)
		write(t, s.dir, "keep/sub/precious", "only copy")
		write(t, s.dir, "file", "only copy too")
		for _, m := range []string{"MOVE", "COPY"} {
			for _, dst := range []string{"keep", "file"} {
				st, _, _ := e.req(t, m, root+"does-not-exist", "", map[string]string{"Destination": e.ts.URL + root + dst, "Overwrite": "T"})
				if st != 404 {
					t.Errorf("%s onto %s: status %d, want 404", m, dst, st)
				}
				if got := read(t, s.dir, "keep/sub/precious"); got != "only copy" {
					t.Fatalf("%s onto %s: precious = %q", m, dst, got)
				}
				if got := read(t, s.dir, "file"); got != "only copy too" {
					t.Fatalf("%s onto %s: file = %q", m, dst, got)
				}
			}
		}
	})
}

// A cached listing must not vouch for a source that has gone: a retried MOVE
// after a lost response is exactly that case.
func TestMoveRetryAfterSourceGoneKeepsDestination(t *testing.T) {
	e := setup(t)
	write(t, e.docs.dir, "src.txt", "new")
	write(t, e.docs.dir, "dst.txt", "the moved content")
	if st, _, _ := e.req(t, "PROPFIND", "/local/docs", "", map[string]string{"Depth": "1"}); st != 207 {
		t.Fatalf("status %d", st)
	}
	os.Remove(filepath.Join(e.docs.dir, "src.txt")) // behind the 2s listing cache
	st, _, _ := e.req(t, "MOVE", "/local/docs/src.txt", "", map[string]string{"Destination": e.ts.URL + "/local/docs/dst.txt", "Overwrite": "T"})
	if st != 404 {
		t.Errorf("status %d, want 404", st)
	}
	if got := read(t, e.docs.dir, "dst.txt"); got != "the moved content" {
		t.Fatalf("dst.txt = %q", got)
	}
}

func TestCopySourceDropKeepsOldDestination(t *testing.T) {
	bothSides(t, func(t *testing.T, e *env, s *fakeShare, root string) {
		write(t, s.dir, "old.txt", "the old destination")
		s.fs.wrapOpen = dropStream
		st, _, _ := e.req(t, "COPY", root+"a.txt", "", map[string]string{"Destination": e.ts.URL + root + "old.txt", "Overwrite": "T"})
		if st < 500 {
			t.Errorf("status %d, want a server error", st)
		}
		if got := read(t, s.dir, "old.txt"); got != "the old destination" {
			t.Fatalf("old.txt = %q", got)
		}
	})
}

// With no old destination the failure must not leave a truncated new one.
func TestCopySourceDropPublishesNothing(t *testing.T) {
	e := setup(t)
	e.docs.fs.wrapOpen = dropStream
	st, _, _ := e.req(t, "COPY", "/local/docs/a.txt", "", map[string]string{"Destination": e.ts.URL + "/local/docs/new.txt"})
	if st < 500 {
		t.Errorf("status %d, want a server error", st)
	}
	gone(t, e.docs.dir, "new.txt")
}

func TestMoveSourceDropKeepsBoth(t *testing.T) {
	bothSides(t, func(t *testing.T, e *env, s *fakeShare, root string) {
		write(t, s.dir, "old.txt", "the old destination")
		s.fs.wrapOpen = dropStream
		st, _, _ := e.req(t, "MOVE", root+"a.txt", "", map[string]string{"Destination": e.ts.URL + root + "old.txt", "Overwrite": "T"})
		if st < 500 {
			t.Errorf("status %d, want a server error", st)
		}
		if got := read(t, s.dir, "old.txt"); got != "the old destination" {
			t.Fatalf("old.txt = %q", got)
		}
		if got := read(t, s.dir, "a.txt"); got != "0123456789" {
			t.Fatalf("a.txt = %q", got)
		}
	})
}

func TestMoveFileOntoFile(t *testing.T) {
	bothSides(t, func(t *testing.T, e *env, s *fakeShare, root string) {
		write(t, s.dir, "report.doc", "version 1")
		st, _, _ := e.req(t, "MOVE", root+"a.txt", "", map[string]string{"Destination": e.ts.URL + root + "report.doc", "Overwrite": "T"})
		if st != 204 {
			t.Fatalf("status %d, want 204", st)
		}
		if got := read(t, s.dir, "report.doc"); got != "0123456789" {
			t.Fatalf("report.doc = %q", got)
		}
		gone(t, s.dir, "a.txt")
		// Listings see both changes at once.
		_, body, _ := e.req(t, "PROPFIND", strings.TrimSuffix(root, "/"), "", map[string]string{"Depth": "1"})
		if strings.Contains(body, "a.txt") {
			t.Error("stale listing shows the source")
		}
	})
}

func TestMoveOntoFileThatCannotBeWrittenKeepsBoth(t *testing.T) {
	e := setup(t)
	write(t, e.docs.dir, "report.doc", "version 1")
	e.docs.fs.putErr = vfs.E(vfs.CodeDiskFull, "full")
	st, _, _ := e.req(t, "MOVE", "/local/docs/a.txt", "", map[string]string{"Destination": e.ts.URL + "/local/docs/report.doc", "Overwrite": "T"})
	if st != 507 {
		t.Errorf("status %d, want 507", st)
	}
	if got := read(t, e.docs.dir, "report.doc"); got != "version 1" {
		t.Fatalf("report.doc = %q", got)
	}
	if got := read(t, e.docs.dir, "a.txt"); got != "0123456789" {
		t.Fatalf("a.txt = %q", got)
	}
}

// What editors do to save atomically: PUT a temp file, MOVE it over the document.
func TestAtomicSaveClient(t *testing.T) {
	bothSides(t, func(t *testing.T, e *env, s *fakeShare, root string) {
		write(t, s.dir, "doc.txt", "old text")
		for i, text := range []string{"second", "third"} {
			if st, _, _ := e.req(t, "PUT", root+".doc.tmp", text, nil); st != 201 {
				t.Fatalf("round %d: PUT %d", i, st)
			}
			st, _, _ := e.req(t, "MOVE", root+".doc.tmp", "", map[string]string{"Destination": e.ts.URL + root + "doc.txt", "Overwrite": "T"})
			if st != 204 {
				t.Fatalf("round %d: MOVE %d", i, st)
			}
			if got := read(t, s.dir, "doc.txt"); got != text {
				t.Fatalf("round %d: doc.txt = %q", i, got)
			}
			gone(t, s.dir, ".doc.tmp")
		}
	})
}

func TestCopyFileOntoFile(t *testing.T) {
	bothSides(t, func(t *testing.T, e *env, s *fakeShare, root string) {
		write(t, s.dir, "b.txt", "old")
		st, _, _ := e.req(t, "COPY", root+"a.txt", "", map[string]string{"Destination": e.ts.URL + root + "b.txt", "Overwrite": "T"})
		if st != 204 {
			t.Fatalf("status %d, want 204", st)
		}
		if got := read(t, s.dir, "b.txt"); got != "0123456789" {
			t.Fatalf("b.txt = %q", got)
		}
		if got := read(t, s.dir, "a.txt"); got != "0123456789" {
			t.Fatalf("a.txt = %q", got)
		}
	})
}

func TestOverwriteInvolvingDirectoryRefused(t *testing.T) {
	bothSides(t, func(t *testing.T, e *env, s *fakeShare, root string) {
		os.MkdirAll(filepath.Join(s.dir, "dir1"), 0o755)
		os.MkdirAll(filepath.Join(s.dir, "dir2"), 0o755)
		write(t, s.dir, "dir1/one", "1")
		write(t, s.dir, "dir2/two", "2")
		write(t, s.dir, "f.txt", "f")
		check := func() {
			t.Helper()
			for name, want := range map[string]string{"dir1/one": "1", "dir2/two": "2", "f.txt": "f", "a.txt": "0123456789"} {
				if got := read(t, s.dir, name); got != want {
					t.Fatalf("%s = %q", name, got)
				}
			}
		}
		for _, m := range []string{"MOVE", "COPY"} {
			for _, c := range [][2]string{{"dir1", "dir2"}, {"dir1", "f.txt"}, {"f.txt", "dir2"}} {
				st, _, _ := e.req(t, m, root+c[0], "", map[string]string{"Destination": e.ts.URL + root + c[1], "Overwrite": "T"})
				if st != 412 {
					t.Errorf("%s %s onto %s: status %d, want 412", m, c[0], c[1], st)
				}
				check()
			}
		}
		// COPY means overwrite when the header is left out.
		st, _, _ := e.req(t, "COPY", root+"dir1", "", map[string]string{"Destination": e.ts.URL + root + "dir2"})
		if st != 412 {
			t.Errorf("COPY without the header: status %d, want 412", st)
		}
		check()
	})
}

func TestOverwriteFalseUnchanged(t *testing.T) {
	bothSides(t, func(t *testing.T, e *env, s *fakeShare, root string) {
		write(t, s.dir, "b.txt", "old")
		os.MkdirAll(filepath.Join(s.dir, "dir1"), 0o755)
		for _, m := range []string{"MOVE", "COPY"} {
			for _, dst := range []string{"b.txt", "dir1"} {
				st, _, _ := e.req(t, m, root+"a.txt", "", map[string]string{"Destination": e.ts.URL + root + dst, "Overwrite": "F"})
				if st != 412 {
					t.Errorf("%s onto %s: status %d, want 412", m, dst, st)
				}
			}
			if st, _, _ := e.req(t, m, root+"a.txt", "", map[string]string{"Destination": e.ts.URL + root + "c-" + m, "Overwrite": "F"}); st != 201 {
				t.Errorf("%s onto a free name: status %d, want 201", m, st)
			}
			write(t, s.dir, "a.txt", "0123456789")
		}
		if got := read(t, s.dir, "b.txt"); got != "old" {
			t.Fatalf("b.txt = %q", got)
		}
	})
}

// Staging must not bypass WebDAV locks: a destination locked by someone else
// stays theirs, and the lock holder can still replace it.
func TestOverwriteRespectsLocks(t *testing.T) {
	e := setup(t)
	write(t, e.docs.dir, "locked.txt", "mine")
	body := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	st, resp, _ := e.req(t, "LOCK", "/local/docs/locked.txt", body, map[string]string{"Timeout": "Second-60"})
	if st != 200 {
		t.Fatalf("LOCK: %d", st)
	}
	found := regexp.MustCompile(`<D:locktoken><D:href>([^<]+)`).FindStringSubmatch(resp)
	if found == nil {
		t.Fatalf("no lock token in %s", resp)
	}
	m := found[1]
	st, _, _ = e.req(t, "MOVE", "/local/docs/a.txt", "", map[string]string{"Destination": e.ts.URL + "/local/docs/locked.txt", "Overwrite": "T"})
	if st != 423 {
		t.Errorf("MOVE without the token: status %d, want 423", st)
	}
	if got := read(t, e.docs.dir, "locked.txt"); got != "mine" {
		t.Fatalf("locked.txt = %q", got)
	}
	st, _, _ = e.req(t, "COPY", "/local/docs/a.txt", "", map[string]string{"Destination": e.ts.URL + "/local/docs/locked.txt", "Overwrite": "T"})
	if st != 423 {
		t.Errorf("COPY without the token: status %d, want 423", st)
	}
	st, _, _ = e.req(t, "COPY", "/local/docs/a.txt", "", map[string]string{"Destination": e.ts.URL + "/local/docs/locked.txt", "Overwrite": "T", "If": "(<" + m + ">)"})
	if st != 204 {
		t.Errorf("with the token: status %d, want 204", st)
	}
	if got := read(t, e.docs.dir, "locked.txt"); got != "0123456789" {
		t.Fatalf("locked.txt = %q", got)
	}
}

// WebDAV applies the same name rules as the Peer API (spec 7).
func TestInvalidNamesRefused(t *testing.T) {
	e := setup(t)
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
	}{
		{"PUT", "/local/docs/bad%FFname", nil},
		{"PUT", "/local/docs/sub/bad%FFname", nil},
		{"MKCOL", "/local/docs/bad%FFdir", nil},
		{"PUT", "/local/docs/bad%FFdir/inner", nil},
		{"COPY", "/local/docs/a.txt", map[string]string{"Destination": e.ts.URL + "/local/docs/bad%FFcopy"}},
		{"MOVE", "/local/docs/a.txt", map[string]string{"Destination": e.ts.URL + "/local/docs/bad%FFmove"}},
	} {
		body := "x"
		if c.method == "MKCOL" {
			body = ""
		}
		st, _, _ := e.req(t, c.method, c.path, body, c.hdr)
		if st != 400 {
			t.Errorf("%s %s: status %d, want 400", c.method, c.path, st)
		}
	}
	ents, err := os.ReadDir(e.docs.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range ents {
		if strings.HasPrefix(de.Name(), "bad") {
			t.Errorf("created %q", de.Name())
		}
	}
	if got := read(t, e.docs.dir, "a.txt"); got != "0123456789" {
		t.Errorf("a.txt = %q", got)
	}
	// A name that is fine stays fine, Korean included.
	if st, _, _ := e.req(t, "PUT", "/local/docs/%ED%95%9C%EA%B8%80.txt", "x", nil); st != 201 {
		t.Errorf("valid name: status %d", st)
	}
}

// The rules are this host's OS's; a peer's share is left to the peer, which may
// allow names this host would not.
func TestNameRulesFollowTheOS(t *testing.T) {
	e := setup(t)
	h := Handler(e.r, tok, 0).(*handler)
	h.fs.goos = "windows"
	ts := httptest.NewServer(h)
	defer ts.Close()
	wenv := &env{ts: ts}
	if st, _, _ := wenv.req(t, "PUT", "/local/docs/a:b", "x", nil); st != 400 {
		t.Errorf("a:b on windows: status %d, want 400", st)
	}
	if st, _, _ := wenv.req(t, "MKCOL", "/local/docs/NUL", "", nil); st != 400 {
		t.Errorf("NUL on windows: status %d, want 400", st)
	}
	if st, _, _ := e.req(t, "PUT", "/local/docs/a:b", "x", nil); st != 201 {
		t.Errorf("a:b on linux: status %d, want 201", st)
	}
	if err := h.fs.checkName(&peerapi.RemoteFS{}, "a:b"); err != nil {
		t.Errorf("a peer's share: %v", err)
	}
}
