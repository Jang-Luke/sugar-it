package davfs

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/vfs"
)

const tok = "s3cret-token"

// countFS counts calls and lets a test act before an Open.
type countFS struct {
	vfs.FS
	stat, list, put atomic.Int64
	beforeOpen      func(n int)
	putErr          error
	listHook        func(ctx context.Context) error
	opens           atomic.Int64
	wrapOpen        func(io.ReadCloser) io.ReadCloser // replaces the stream Open returns
}

func (c *countFS) Stat(ctx context.Context, p string) (vfs.Entry, error) {
	c.stat.Add(1)
	return c.FS.Stat(ctx, p)
}
func (c *countFS) List(ctx context.Context, p string) ([]vfs.Entry, error) {
	c.list.Add(1)
	if c.listHook != nil {
		if err := c.listHook(ctx); err != nil {
			return nil, err
		}
	}
	return c.FS.List(ctx, p)
}
func (c *countFS) Put(ctx context.Context, p string, r io.Reader, o vfs.PutOptions) (vfs.Entry, error) {
	c.put.Add(1)
	if c.putErr != nil {
		io.Copy(io.Discard, r)
		return vfs.Entry{}, c.putErr
	}
	return c.FS.Put(ctx, p, r, o)
}
func (c *countFS) Open(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error) {
	n := int(c.opens.Add(1))
	if c.beforeOpen != nil {
		c.beforeOpen(n)
	}
	rc, e, err := c.FS.Open(ctx, p, off, etag)
	if err == nil && c.wrapOpen != nil {
		rc = c.wrapOpen(rc)
	}
	return rc, e, err
}

type fakeShare struct {
	fs  *countFS
	ro  bool
	dir string
}

type fakePeer struct {
	shares  map[string]*fakeShare
	offline bool
}

type fakeRouter struct{ peers map[string]*fakePeer }

func (f *fakeRouter) PeerNames() []string {
	var n []string
	for k := range f.peers {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func (f *fakeRouter) peer(name string) (*fakePeer, error) {
	p := f.peers[name]
	if p == nil {
		return nil, vfs.E(vfs.CodePeerNotFound, "no peer %q", name)
	}
	if p.offline {
		return nil, vfs.E(vfs.CodePeerOffline, "peer is offline")
	}
	return p, nil
}

func (f *fakeRouter) Shares(ctx context.Context, peer string) ([]peerapi.ShareInfo, error) {
	p, err := f.peer(peer)
	if err != nil {
		return nil, err
	}
	var out []peerapi.ShareInfo
	for n, s := range p.shares {
		out = append(out, peerapi.ShareInfo{Name: n, ReadOnly: s.ro})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeRouter) FS(ctx context.Context, peer, share string) (vfs.FS, error) {
	p, err := f.peer(peer)
	if err != nil {
		return nil, err
	}
	s := p.shares[share]
	if s == nil {
		return nil, vfs.E(vfs.CodeShareNotFound, "no share %q", share)
	}
	return s.fs, nil
}

func newShare(t *testing.T, name string, ro bool) *fakeShare {
	t.Helper()
	dir := t.TempDir()
	l, err := shares.Open(shares.Spec{Name: name, Path: dir, ReadOnly: ro})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return &fakeShare{fs: &countFS{FS: l}, ro: ro, dir: dir}
}

var client = &http.Client{Timeout: 10 * time.Second}

type env struct {
	r    *fakeRouter
	ts   *httptest.Server
	docs *fakeShare
	ro   *fakeShare
	pics *fakeShare
	bob  *fakePeer
	off  *fakePeer
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{docs: newShare(t, "docs", false), ro: newShare(t, "ro", true), pics: newShare(t, "pics", false)}
	e.bob = &fakePeer{shares: map[string]*fakeShare{"pics": e.pics}}
	e.off = &fakePeer{shares: map[string]*fakeShare{"x": newShare(t, "x", false)}, offline: true}
	e.r = &fakeRouter{peers: map[string]*fakePeer{
		"local": {shares: map[string]*fakeShare{"docs": e.docs, "ro": e.ro}},
		"bob":   e.bob,
		"off":   e.off,
	}}
	write(t, e.docs.dir, "a.txt", "0123456789")
	os.MkdirAll(filepath.Join(e.docs.dir, "sub"), 0o755)
	write(t, e.ro.dir, "r.txt", "readonly")
	e.ts = httptest.NewServer(Handler(e.r, tok, 2*time.Second))
	t.Cleanup(e.ts.Close)
	return e
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *env) req(t *testing.T, method, path, body string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("sugar-it", tok)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestAuthRequired(t *testing.T) {
	e := setup(t)
	for name, tc := range map[string]struct{ user, pass string }{
		"none": {}, "wrong password": {"sugar-it", "nope"}, "wrong user": {"root", tok}, "empty": {"sugar-it", ""},
	} {
		req, _ := http.NewRequest("PROPFIND", e.ts.URL+"/", nil)
		if name != "none" {
			req.SetBasicAuth(tc.user, tc.pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s: status %d, challenge %q", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	if st, _, _ := e.req(t, "PROPFIND", "/", "", map[string]string{"Depth": "0"}); st != 207 {
		t.Errorf("valid credentials: %d", st)
	}
}

func TestEmptyTokenRejectsEverything(t *testing.T) {
	e := setup(t)
	ts := httptest.NewServer(Handler(e.r, "", time.Second))
	defer ts.Close()
	req, _ := http.NewRequest("PROPFIND", ts.URL+"/", nil)
	req.SetBasicAuth("sugar-it", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("empty token accepted: %d", resp.StatusCode)
	}
}

func TestNonLoopbackHostRejected(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("PROPFIND", e.ts.URL+"/", nil)
	req.Host = "evil.example.com:7445"
	req.SetBasicAuth("sugar-it", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("rebinding host got %d", resp.StatusCode)
	}
	if strings.Contains(string(b), tok) {
		t.Fatal("token echoed")
	}
	for _, h := range []string{"localhost:7445", "[::1]:7445", "127.0.0.1"} {
		req, _ := http.NewRequest("PROPFIND", e.ts.URL+"/", nil)
		req.Host = h
		req.Header.Set("Depth", "0")
		req.SetBasicAuth("sugar-it", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 207 {
			t.Errorf("host %s: %d", h, resp.StatusCode)
		}
	}
}

func TestPropfindRootListsPeers(t *testing.T) {
	e := setup(t)
	st, body, _ := e.req(t, "PROPFIND", "/", "", map[string]string{"Depth": "1"})
	if st != 207 {
		t.Fatalf("status %d", st)
	}
	for _, p := range []string{"/local/", "/bob/", "/off/"} { // offline peers still listed
		if !strings.Contains(body, ">"+p+"<") {
			t.Errorf("missing %s in %s", p, body)
		}
	}
}

func TestPropfindPeerListsShares(t *testing.T) {
	e := setup(t)
	st, body, _ := e.req(t, "PROPFIND", "/local", "", map[string]string{"Depth": "1"})
	if st != 207 || !strings.Contains(body, ">/local/docs/<") || !strings.Contains(body, ">/local/ro/<") {
		t.Fatalf("%d %s", st, body)
	}
	st, body, _ = e.req(t, "PROPFIND", "/local/docs", "", map[string]string{"Depth": "1"})
	if st != 207 || !strings.Contains(body, "a.txt") || !strings.Contains(body, "sub") {
		t.Fatalf("%d %s", st, body)
	}
	if st, _, _ = e.req(t, "PROPFIND", "/nobody", "", map[string]string{"Depth": "0"}); st != 404 {
		t.Errorf("unknown peer: %d", st)
	}
}

func TestVirtualDirsReadOnly(t *testing.T) {
	e := setup(t)
	for _, tc := range []struct{ m, p string }{
		{"PUT", "/f.txt"}, {"MKCOL", "/newpeer"}, {"DELETE", "/local"}, {"DELETE", "/"},
		{"PUT", "/local/f.txt"}, {"MKCOL", "/local/newshare"}, {"DELETE", "/local/docs"},
		{"MOVE", "/local/docs"},
	} {
		hdr := map[string]string{}
		if tc.m == "MOVE" {
			hdr["Destination"] = e.ts.URL + "/local/other"
		}
		st, _, _ := e.req(t, tc.m, tc.p, "x", hdr)
		if st < 400 || st >= 500 {
			t.Errorf("%s %s: %d", tc.m, tc.p, st)
		}
	}
	if _, err := os.Stat(filepath.Join(e.docs.dir, "newshare")); err == nil {
		t.Error("created a directory")
	}
	if _, err := os.Stat(e.docs.dir); err != nil {
		t.Error("share removed")
	}
}

func TestGetRange(t *testing.T) {
	e := setup(t)
	st, body, _ := e.req(t, "GET", "/local/docs/a.txt", "", map[string]string{"Range": "bytes=2-3"})
	if st != 206 || body != "23" {
		t.Fatalf("%d %q", st, body)
	}
	st, body, _ = e.req(t, "GET", "/local/docs/a.txt", "", nil)
	if st != 200 || body != "0123456789" {
		t.Fatalf("%d %q", st, body)
	}
	if st, _, _ = e.req(t, "GET", "/local/docs/missing", "", nil); st != 404 {
		t.Errorf("missing: %d", st)
	}
}

func TestReadSourceChangedMidRead(t *testing.T) {
	e := setup(t)
	e.docs.fs.beforeOpen = func(n int) {
		if n == 2 { // the re-open for the Range seek
			write(t, e.docs.dir, "a.txt", "ABCDEFGHIJKLMNOP")
		}
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/local/docs/a.txt", nil)
	req.SetBasicAuth("sugar-it", tok)
	req.Header.Set("Range", "bytes=2-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return // aborted before headers: fine
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil && len(b) > 0 {
		t.Fatalf("served bytes after the source changed: %q", b)
	}
}

func TestPutRoundTrip(t *testing.T) {
	e := setup(t)
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("PUT", e.ts.URL+"/local/docs/up.bin", pr)
	req.SetBasicAuth("sugar-it", tok)
	req.ContentLength = 10
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			done <- nil
			return
		}
		done <- resp
	}()
	pw.Write([]byte("hello"))
	time.Sleep(150 * time.Millisecond)
	ents, _ := os.ReadDir(e.docs.dir)
	for _, d := range ents {
		if d.Name() != "a.txt" && d.Name() != "sub" {
			t.Fatalf("incomplete file %q visible before Close", d.Name())
		}
	}
	pw.Write([]byte("world"))
	pw.Close()
	resp := <-done
	if resp == nil {
		return
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	if st, body, _ := e.req(t, "GET", "/local/docs/up.bin", "", nil); st != 200 || body != "helloworld" {
		t.Fatalf("%d %q", st, body)
	}
	// Overwrite an existing file.
	if st, _, _ := e.req(t, "PUT", "/local/docs/up.bin", "second", nil); st != 201 && st != 204 {
		t.Fatalf("overwrite: %d", st)
	}
	if b, _ := os.ReadFile(filepath.Join(e.docs.dir, "up.bin")); string(b) != "second" {
		t.Fatalf("content %q", b)
	}
	// Missing parent directory.
	if st, _, _ := e.req(t, "PUT", "/local/docs/nodir/x", "x", nil); st != 409 {
		t.Errorf("missing parent: %d", st)
	}
}

func TestPutReadOnlyShare(t *testing.T) {
	e := setup(t)
	st, _, _ := e.req(t, "PUT", "/local/ro/new.txt", "data", nil)
	if st != 403 {
		t.Fatalf("status %d", st)
	}
	if _, err := os.Stat(filepath.Join(e.ro.dir, "new.txt")); err == nil {
		t.Fatal("file created in read-only share")
	}
	for _, m := range []string{"DELETE", "MKCOL"} {
		p := "/local/ro/r.txt"
		if m == "MKCOL" {
			p = "/local/ro/d"
		}
		if st, _, _ := e.req(t, m, p, "", nil); st != 403 {
			t.Errorf("%s: %d", m, st)
		}
	}
	if _, err := os.Stat(filepath.Join(e.ro.dir, "r.txt")); err != nil {
		t.Error("file removed from read-only share")
	}
}

func TestMkcolAndDelete(t *testing.T) {
	e := setup(t)
	if st, _, _ := e.req(t, "MKCOL", "/local/docs/newdir", "", nil); st != 201 {
		t.Fatalf("mkcol %d", st)
	}
	if st, _, _ := e.req(t, "MKCOL", "/local/docs/newdir", "", nil); st != 405 {
		t.Errorf("mkcol existing %d", st)
	}
	if st, _, _ := e.req(t, "DELETE", "/local/docs/newdir", "", nil); st != 204 {
		t.Fatalf("delete %d", st)
	}
	if st, _, _ := e.req(t, "DELETE", "/local/docs/newdir", "", nil); st != 404 {
		t.Errorf("delete missing %d", st)
	}
}

func TestMoveWithinShare(t *testing.T) {
	e := setup(t)
	st, _, _ := e.req(t, "MOVE", "/local/docs/a.txt", "", map[string]string{"Destination": e.ts.URL + "/local/docs/sub/b.txt", "Overwrite": "T"})
	if st != 201 {
		t.Fatalf("status %d", st)
	}
	if _, err := os.Stat(filepath.Join(e.docs.dir, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.docs.dir, "a.txt")); err == nil {
		t.Fatal("source remains")
	}
	// The listing must reflect both parents at once.
	_, body, _ := e.req(t, "PROPFIND", "/local/docs", "", map[string]string{"Depth": "1"})
	if strings.Contains(body, "a.txt") {
		t.Error("stale root listing")
	}
	_, body, _ = e.req(t, "PROPFIND", "/local/docs/sub", "", map[string]string{"Depth": "1"})
	if !strings.Contains(body, "b.txt") {
		t.Error("stale destination listing")
	}
}

func TestMoveAcrossHostsRejected(t *testing.T) {
	e := setup(t)
	for _, dst := range []string{"/bob/pics/a.txt", "/local/ro/a.txt", "/local/docs2/a.txt"} {
		st, _, _ := e.req(t, "MOVE", "/local/docs/a.txt", "", map[string]string{"Destination": e.ts.URL + dst, "Overwrite": "T"})
		if st != 502 && st != 403 && st != 404 {
			t.Errorf("%s: %d", dst, st)
		}
	}
	st, _, _ := e.req(t, "MOVE", "/local/docs/a.txt", "", map[string]string{"Destination": e.ts.URL + "/bob/pics/a.txt", "Overwrite": "T"})
	if st != 502 {
		t.Fatalf("status %d", st)
	}
	if _, err := os.Stat(filepath.Join(e.docs.dir, "a.txt")); err != nil {
		t.Fatal("source lost")
	}
	if _, err := os.Stat(filepath.Join(e.pics.dir, "a.txt")); err == nil {
		t.Fatal("destination created")
	}
}

func TestPeerOfflineNotHang(t *testing.T) {
	e := setup(t)
	start := time.Now()
	st, _, _ := e.req(t, "PROPFIND", "/off/x", "", map[string]string{"Depth": "1"})
	if st < 500 || st > 599 || time.Since(start) > 6*time.Second {
		t.Fatalf("status %d after %v", st, time.Since(start))
	}
	if st, _, _ = e.req(t, "PROPFIND", "/off", "", map[string]string{"Depth": "1"}); st < 500 || st > 599 {
		t.Errorf("peer dir: %d", st)
	}
	_, body, _ := e.req(t, "PROPFIND", "/", "", map[string]string{"Depth": "1"})
	if !strings.Contains(body, ">/bob/<") || !strings.Contains(body, ">/off/<") {
		t.Errorf("root lost peers: %s", body)
	}
}

func TestStatCacheReducesCalls(t *testing.T) {
	e := setup(t)
	for i := 0; i < 100; i++ {
		write(t, e.docs.dir, fmt.Sprintf("f%03d.txt", i), "x")
	}
	for round := 0; round < 2; round++ {
		st, body, _ := e.req(t, "PROPFIND", "/local/docs", "", map[string]string{"Depth": "1"})
		if st != 207 || !strings.Contains(body, "f099.txt") {
			t.Fatalf("%d", st)
		}
	}
	if n := e.docs.fs.stat.Load(); n != 0 {
		t.Errorf("FS.Stat called %d times", n)
	}
	if n := e.docs.fs.list.Load(); n > 2 {
		t.Errorf("FS.List called %d times for two PROPFINDs", n)
	}
}

func TestCacheInvalidatedOnWrite(t *testing.T) {
	e := setup(t)
	list := func() string {
		_, b, _ := e.req(t, "PROPFIND", "/local/docs", "", map[string]string{"Depth": "1"})
		return b
	}
	if strings.Contains(list(), "new.txt") {
		t.Fatal("precondition")
	}
	if st, _, _ := e.req(t, "PUT", "/local/docs/new.txt", "n", nil); st != 201 {
		t.Fatal(st)
	}
	if !strings.Contains(list(), "new.txt") {
		t.Error("PUT not visible")
	}
	e.req(t, "MKCOL", "/local/docs/dd", "", nil)
	if !strings.Contains(list(), "dd") {
		t.Error("MKCOL not visible")
	}
	e.req(t, "DELETE", "/local/docs/new.txt", "", nil)
	if strings.Contains(list(), "new.txt") {
		t.Error("DELETE not visible")
	}
}

func TestPutSpoolCleanedUp(t *testing.T) {
	e := setup(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var wg sync.WaitGroup
	inner := Handler(e.r, tok, time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()
	wg.Add(1)
	c, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("sugar-it:" + tok))
	fmt.Fprintf(c, "PUT /local/docs/trunc.bin HTTP/1.1\r\nHost: %s\r\nAuthorization: Basic %s\r\nContent-Length: 100\r\n\r\n12345", ts.Listener.Addr(), auth)
	c.Close() // the upload is cut off
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
	if _, err := os.Stat(filepath.Join(e.docs.dir, "trunc.bin")); err == nil {
		t.Fatal("truncated file appeared in the share")
	}
	if n := e.docs.fs.put.Load(); n != 0 {
		t.Fatalf("Put called %d times for a broken upload", n)
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Fatalf("spool files left: %v", ents)
	}
	// A successful upload also leaves nothing behind.
	e2 := httptest.NewServer(Handler(e.r, tok, time.Second))
	defer e2.Close()
	req, _ := http.NewRequest("PUT", e2.URL+"/local/docs/ok.bin", strings.NewReader("ok"))
	req.SetBasicAuth("sugar-it", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Fatalf("spool files left after success: %v", ents)
	}
}

func TestSymlinkHidden(t *testing.T) {
	e := setup(t)
	outside := t.TempDir()
	write(t, outside, "secret.txt", "top secret")
	if err := os.Symlink(outside, filepath.Join(e.docs.dir, "escape")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(e.docs.dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	_, body, _ := e.req(t, "PROPFIND", "/local/docs", "", map[string]string{"Depth": "1"})
	if strings.Contains(body, "escape") || strings.Contains(body, "link.txt") {
		t.Errorf("symlink listed: %s", body)
	}
	for _, p := range []string{"/local/docs/link.txt", "/local/docs/escape/secret.txt"} {
		if st, b, _ := e.req(t, "GET", p, "", nil); st != 404 || strings.Contains(b, "secret") {
			t.Errorf("%s: %d %q", p, st, b)
		}
	}
}

func TestMoveCopyOntoRelatedPathRefused(t *testing.T) {
	e := setup(t)
	os.MkdirAll(filepath.Join(e.docs.dir, "sub", "deep"), 0o755)
	write(t, e.docs.dir, "sub/f.txt", "f")
	write(t, e.docs.dir, "sub/deep/g.txt", "g")
	cases := []struct{ m, src, dst string }{
		{"MOVE", "sub/f.txt", "sub"},      // onto the parent
		{"MOVE", "sub/deep/g.txt", "sub"}, // onto an ancestor
		{"MOVE", "sub", "sub/deep"},       // into itself
		{"MOVE", "sub", "sub/new"},        // into itself, new name
		{"MOVE", "sub", "sub"},            // onto itself
		{"MOVE", "sub", "SUB/new"},        // case-folded spelling
		{"COPY", "sub", "sub/copy"},       // would recurse forever
		{"COPY", "sub/f.txt", "sub"},      // onto the parent
		{"COPY", "sub", "sub/deep"},
		{"COPY", "sub", "Sub/copy"},
	}
	for _, c := range cases {
		for _, ow := range []string{"T", "F"} {
			st, _, _ := e.req(t, c.m, "/local/docs/"+c.src, "", map[string]string{
				"Destination": e.ts.URL + "/local/docs/" + c.dst, "Overwrite": ow})
			if st != 403 && st != 409 {
				t.Errorf("%s %s -> %s (Overwrite %s): %d", c.m, c.src, c.dst, ow, st)
			}
			for _, f := range []string{"sub/f.txt", "sub/deep/g.txt"} {
				if _, err := os.Stat(filepath.Join(e.docs.dir, f)); err != nil {
					t.Fatalf("%s %s -> %s (Overwrite %s) destroyed %s", c.m, c.src, c.dst, ow, f)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(e.docs.dir, "sub", "copy")); err == nil {
		t.Error("copy created inside the source")
	}
}

func TestMetadataCallsTimeOut(t *testing.T) {
	old := metadataTimeout
	metadataTimeout = 300 * time.Millisecond
	defer func() { metadataTimeout = old }()
	e := setup(t)
	e.docs.fs.listHook = func(ctx context.Context) error {
		<-ctx.Done() // a peer that stopped answering
		return ctx.Err()
	}
	start := time.Now()
	st, _, _ := e.req(t, "PROPFIND", "/local/docs", "", map[string]string{"Depth": "1"})
	if st < 500 || st > 599 || time.Since(start) > 3*time.Second {
		t.Fatalf("status %d after %v", st, time.Since(start))
	}
}

func TestPutCloseErrorStatus(t *testing.T) {
	for code, want := range map[string]int{
		vfs.CodeInvalidName: 400, vfs.CodeConflict: 409, vfs.CodePeerOffline: 502, vfs.CodeDiskFull: 507,
		vfs.CodeReadOnly: 403, vfs.CodeChanged: 412,
	} {
		e := setup(t)
		e.docs.fs.putErr = vfs.E(code, "boom")
		if st, _, _ := e.req(t, "PUT", "/local/docs/x.bin", "data", nil); st != want {
			t.Errorf("%s: status %d, want %d", code, st, want)
		}
		if _, err := os.Stat(filepath.Join(e.docs.dir, "x.bin")); err == nil {
			t.Errorf("%s: file created", code)
		}
	}
}

func TestMetadataTimeoutOutsidePreflight(t *testing.T) {
	old := metadataTimeout
	metadataTimeout = 300 * time.Millisecond
	defer func() { metadataTimeout = old }()
	e := setup(t)
	os.MkdirAll(filepath.Join(e.docs.dir, "d"), 0o755)
	write(t, e.docs.dir, "d/f.txt", "f")
	e.docs.fs.listHook = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	e.req(t, "COPY", "/local/docs/d", "", map[string]string{"Destination": e.ts.URL + "/local/docs/d2"})
	if time.Since(start) > 3*time.Second {
		t.Fatalf("COPY hung for %v", time.Since(start))
	}
}
