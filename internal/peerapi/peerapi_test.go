package peerapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"sugarintegration/internal/identity"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

var bg = context.Background()

// ---- fixtures ----

func newID(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type fakeBackend struct {
	mu      sync.Mutex
	members map[string]bool
	shares  map[string]vfs.FS
	ro      map[string]bool
	hits    int

	pairCaller string
	pairReq    PairRequest
	syncCaller string
	syncIn     []mesh.Member
	submitBy   string
	jobs       []transfer.Job
	canceled   []string
	retried    []string
}

func newBackend(members ...string) *fakeBackend {
	b := &fakeBackend{members: map[string]bool{}, shares: map[string]vfs.FS{}, ro: map[string]bool{}}
	for _, m := range members {
		b.members[m] = true
	}
	return b
}

func (b *fakeBackend) hit() { b.mu.Lock(); b.hits++; b.mu.Unlock() }
func (b *fakeBackend) hitCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits
}
func (b *fakeBackend) setMember(id string, ok bool) {
	b.mu.Lock()
	b.members[id] = ok
	b.mu.Unlock()
}

func (b *fakeBackend) IsMember(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hits++
	return b.members[id]
}
func (b *fakeBackend) Info() Info {
	b.hit()
	return Info{ID: "self-id", Name: "self", Protocol: ProtocolMajor}
}
func (b *fakeBackend) Shares() []ShareInfo {
	b.hit()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ShareInfo
	for n := range b.shares {
		out = append(out, ShareInfo{Name: n, ReadOnly: b.ro[n]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (b *fakeBackend) ShareFS(name string) (vfs.FS, error) {
	b.hit()
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.shares[name]
	if !ok {
		return nil, vfs.E(vfs.CodeShareNotFound, "no share %q", name)
	}
	return f, nil
}
func (b *fakeBackend) Pair(_ context.Context, caller string, r PairRequest) (PairResponse, error) {
	b.hit()
	b.mu.Lock()
	b.pairCaller, b.pairReq = caller, r
	b.mu.Unlock()
	if r.Token == "bad" {
		return PairResponse{}, vfs.E(vfs.CodePeerUnauthorized, "bad token")
	}
	return PairResponse{
		Self:    mesh.Member{ID: "self-id", Name: "self", Addrs: []string{"1.2.3.4:7443"}, Generation: 3},
		Members: []mesh.Member{{ID: "x", Name: "ünï", Generation: 9, Removed: true}},
	}, nil
}
func (b *fakeBackend) SyncMembers(_ context.Context, caller string, remote []mesh.Member) ([]mesh.Member, error) {
	b.hit()
	b.mu.Lock()
	b.syncCaller, b.syncIn = caller, remote
	b.mu.Unlock()
	return append(remote, mesh.Member{ID: "added", Name: "added", Addrs: []string{"a:1"}, Generation: 1}), nil
}
func (b *fakeBackend) SubmitJob(caller string, s transfer.Spec) (transfer.Job, error) {
	b.hit()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.submitBy = caller
	j := transfer.Job{ID: fmt.Sprintf("j%d", len(b.jobs)+1), Host: "self-id", Origin: caller, Spec: s, State: transfer.StateQueued,
		Created: time.Unix(1700000000, 123456789).UTC()}
	b.jobs = append(b.jobs, j)
	return j, nil
}
func (b *fakeBackend) ListJobs(origin string) []transfer.Job {
	b.hit()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []transfer.Job
	for _, j := range b.jobs {
		if origin == "" || j.Origin == origin {
			out = append(out, j)
		}
	}
	return out
}
func (b *fakeBackend) CancelJob(id string) error {
	b.hit()
	if id != "j1" && id != "a b/c" {
		return vfs.E(vfs.CodeNotFound, "no job")
	}
	b.mu.Lock()
	b.canceled = append(b.canceled, id)
	b.mu.Unlock()
	return nil
}
func (b *fakeBackend) RetryJob(id string) error {
	b.hit()
	if id != "j1" {
		return vfs.E(vfs.CodeNotFound, "no job")
	}
	b.mu.Lock()
	b.retried = append(b.retried, id)
	b.mu.Unlock()
	return nil
}

// hookFS lets a test replace single operations of a real FS.
type hookFS struct {
	vfs.FS
	open func(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error)
	hash func(ctx context.Context, p string) (string, error)
	put  func(ctx context.Context, p string, r io.Reader, o vfs.PutOptions) (vfs.Entry, error)
}

func (h *hookFS) Open(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error) {
	if h.open != nil {
		return h.open(ctx, p, off, etag)
	}
	return h.FS.Open(ctx, p, off, etag)
}
func (h *hookFS) Hash(ctx context.Context, p string) (string, error) {
	if h.hash != nil {
		return h.hash(ctx, p)
	}
	return h.FS.Hash(ctx, p)
}
func (h *hookFS) Put(ctx context.Context, p string, r io.Reader, o vfs.PutOptions) (vfs.Entry, error) {
	if h.put != nil {
		return h.put(ctx, p, r, o)
	}
	return h.FS.Put(ctx, p, r, o)
}

func localShare(t *testing.T, ro bool) (*shares.LocalFS, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := shares.Open(shares.Spec{Name: "s", Path: dir, ReadOnly: ro})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, dir
}

type env struct {
	srvID, cliID *identity.Identity
	b            *fakeBackend
	srv          *http.Server
	addr         string
	c            *Client
	logs         syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func start(t *testing.T, goos string) *env {
	t.Helper()
	e := &env{srvID: newID(t), cliID: newID(t)}
	e.b = newBackend(e.cliID.ID)
	srv := newServer(e.srvID, e.b, goos)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.ErrorLog = log.New(&filterWriter{w: &e.logs}, "", 0) // same filter as production, captured
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	e.srv, e.addr = srv, ln.Addr().String()
	e.c = NewClient(e.cliID, e.srvID.ID, []string{e.addr})
	return e
}

func (e *env) share(name string, fsys vfs.FS, ro bool) {
	e.b.mu.Lock()
	e.b.shares[name] = fsys
	e.b.ro[name] = ro
	e.b.mu.Unlock()
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := vfs.CodeOf(err); got != code {
		t.Fatalf("code = %q (%v), want %q", got, err, code)
	}
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- authentication ----

func TestNonMemberForbidden(t *testing.T) {
	e := start(t, "linux")
	l, _ := localShare(t, false)
	e.share("s", l, false)
	e.b.setMember(e.cliID.ID, false)

	_, err := e.c.Shares(bg)
	wantCode(t, err, vfs.CodePeerUnauthorized)
	_, err = e.c.FS("s").List(bg, "")
	wantCode(t, err, vfs.CodePeerUnauthorized)
	_, err = e.c.Info(bg)
	wantCode(t, err, vfs.CodePeerUnauthorized)
	_, err = e.c.SyncMembers(bg, nil)
	wantCode(t, err, vfs.CodePeerUnauthorized)
	_, err = e.c.ListJobs(bg, "")
	wantCode(t, err, vfs.CodePeerUnauthorized)

	// Pair is reachable, and sees the caller's certificate ID, not a claimed one.
	resp, err := e.c.Pair(bg, PairRequest{Token: "t", Name: "n", Addrs: []string{"9.9.9.9:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Self.ID != "self-id" || len(resp.Members) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if e.b.pairCaller != e.cliID.ID || e.b.pairReq.Token != "t" || e.b.pairReq.Name != "n" ||
		!reflect.DeepEqual(e.b.pairReq.Addrs, []string{"9.9.9.9:1"}) {
		t.Fatalf("pair got caller=%q req=%+v", e.b.pairCaller, e.b.pairReq)
	}
	// Backend errors on pair keep their code.
	_, err = e.c.Pair(bg, PairRequest{Token: "bad"})
	wantCode(t, err, vfs.CodePeerUnauthorized)
}

func TestRemovedMemberForbiddenImmediately(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	write(t, filepath.Join(dir, "a"), "x")
	e.share("s", l, false)
	if _, err := e.c.FS("s").List(bg, ""); err != nil {
		t.Fatal(err)
	}
	e.b.setMember(e.cliID.ID, false)
	_, err := e.c.FS("s").List(bg, "") // same pooled connection
	wantCode(t, err, vfs.CodePeerUnauthorized)
}

func TestPinnedFingerprintRejected(t *testing.T) {
	e := start(t, "linux")
	other := newID(t)
	c := NewClient(e.cliID, other.ID, []string{e.addr})
	_, err := c.Info(bg)
	if err == nil {
		t.Fatal("connected to a server whose identity is not pinned")
	}
	wantCode(t, err, vfs.CodePeerOffline)
	if n := e.b.hitCount(); n != 0 {
		t.Fatalf("backend saw %d calls from an unpinned connection", n)
	}
}

func TestPinRunsOnResumedSessions(t *testing.T) {
	e := start(t, "linux")
	cache := tls.NewLRUClientSessionCache(4)
	dial := func(cfg *tls.Config) (*tls.Conn, error) {
		cfg = cfg.Clone()
		cfg.ClientSessionCache = cache
		cfg.ServerName = "peer.invalid"
		cfg.NextProtos = nil
		return tls.Dial("tcp", e.addr, cfg)
	}
	prime := func() {
		conn, err := dial(clientTLSConfig(e.cliID, e.srvID.ID))
		if err != nil {
			t.Fatal(err)
		}
		// The ticket arrives after the handshake; one exchange delivers it.
		io.WriteString(conn, "GET /v1/info HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		io.Copy(io.Discard, conn)
		conn.Close()
	}
	prime()

	// A config pinned to someone else must still be refused even if the
	// session would resume (VerifyPeerCertificate is skipped on resumption).
	cfg := clientTLSConfig(e.cliID, newID(t).ID)
	conn, err := dial(cfg)
	if err == nil {
		resumed := conn.ConnectionState().DidResume
		conn.Close()
		t.Fatalf("pin bypassed (resumed=%v)", resumed)
	}
	// And the right pin does resume, proving the test exercises resumption.
	prime()
	conn, err = dial(clientTLSConfig(e.cliID, e.srvID.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !conn.ConnectionState().DidResume {
		t.Fatal("session did not resume; test cannot prove the property")
	}
}

func TestHTTP2Negotiated(t *testing.T) {
	e := start(t, "linux")
	resp, err := e.c.hc.Get(e.c.base + "/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.TLS == nil || resp.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("proto = %s alpn = %v", resp.Proto, resp.TLS)
	}
	if resp.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("tls version %x", resp.TLS.Version)
	}
}

func TestServerRequiresClientCertAndTLS13(t *testing.T) {
	e := start(t, "linux")
	// No client certificate.
	conn, err := tls.Dial("tcp", e.addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err == nil {
		_, err = conn.Read(make([]byte, 1)) // TLS 1.3 reports the rejection on first use
		conn.Close()
	}
	if err == nil {
		t.Fatal("handshake without a client certificate succeeded")
	}
	// TLS 1.2 only.
	_, err = tls.Dial("tcp", e.addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{e.cliID.Cert}})
	if err == nil {
		t.Fatal("TLS 1.2 accepted")
	}
	if n := e.b.hitCount(); n != 0 {
		t.Fatalf("backend hit %d times", n)
	}
}

// ---- remote filesystem ----

func seed(t *testing.T, dir string) {
	t.Helper()
	write(t, filepath.Join(dir, "a.txt"), "hello world")
	write(t, filepath.Join(dir, "sub", "b.txt"), "bee")
	write(t, filepath.Join(dir, "sub", "deep", "c.txt"), "sea")
	write(t, filepath.Join(dir, "empty.txt"), "")
	if err := os.MkdirAll(filepath.Join(dir, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(1700000000, 123456789)
	for _, f := range []string{"a.txt", "sub/b.txt", "sub/deep/c.txt", "empty.txt"} {
		if err := os.Chtimes(filepath.Join(dir, f), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
}

func descr(e vfs.Entry, withTime bool) string {
	s := fmt.Sprintf("%s|%s|%s|%d|%o", e.Name, e.Path, e.Type, e.Size, e.Mode)
	if e.Type == vfs.TypeFile {
		s += "|" + e.ETag
		if withTime {
			s += fmt.Sprintf("|%d", e.ModTime.UnixNano())
		}
	}
	return s
}

func res(detail string, err error) string {
	return detail + " => " + vfs.CodeOf(err)
}

type convCase struct {
	name  string
	share string
	run   func(f vfs.FS) string
}

func convCases() []convCase {
	fixed := time.Unix(1600000000, 5)
	list := func(p string) func(f vfs.FS) string {
		return func(f vfs.FS) string {
			es, err := f.List(bg, p)
			var out []string
			for _, e := range es {
				out = append(out, descr(e, true))
			}
			sort.Strings(out)
			return res(strings.Join(out, ";"), err)
		}
	}
	stat := func(p string) func(f vfs.FS) string {
		return func(f vfs.FS) string {
			e, err := f.Stat(bg, p)
			return res(descr(e, true), err)
		}
	}
	open := func(p string, off int64, etag string) func(f vfs.FS) string {
		return func(f vfs.FS) string {
			rc, e, err := f.Open(bg, p, off, etag)
			if err != nil {
				return res("", err)
			}
			b, rerr := io.ReadAll(rc)
			rc.Close()
			if rerr != nil {
				return res("read error "+rerr.Error(), rerr)
			}
			return res(fmt.Sprintf("%q %s", b, descr(e, true)), nil)
		}
	}
	hash := func(p string) func(f vfs.FS) string {
		return func(f vfs.FS) string { h, err := f.Hash(bg, p); return res(h, err) }
	}
	put := func(p, body string, o vfs.PutOptions) func(f vfs.FS) string {
		return func(f vfs.FS) string {
			e, err := f.Put(bg, p, strings.NewReader(body), o)
			after := ""
			if err == nil {
				after = descr(e, !o.ModTime.IsZero())
			}
			// What is left behind is part of the contract.
			st, serr := f.Stat(bg, p)
			return res(after+" then "+descr(st, !o.ModTime.IsZero())+"/"+vfs.CodeOf(serr), err)
		}
	}
	other := func(fn func(f vfs.FS) error, check string) func(f vfs.FS) string {
		return func(f vfs.FS) string {
			err := fn(f)
			st, serr := f.Stat(bg, check)
			return res(descr(st, true)+"/"+vfs.CodeOf(serr), err)
		}
	}
	sum := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9" // sha256("hello world")
	return []convCase{
		{"list root", "rw", list("")},
		{"list sub", "rw", list("sub")},
		{"list missing", "rw", list("nope")},
		{"list file", "rw", list("a.txt")},
		{"list traversal", "rw", list("../x")},
		{"list part dir", "rw", list(vfs.PartDir)},
		{"stat file", "rw", stat("a.txt")},
		{"stat dir", "rw", stat("sub")},
		{"stat root", "rw", stat("")},
		{"stat missing", "rw", stat("zz")},
		{"open all", "rw", open("a.txt", 0, "")},
		{"open offset", "rw", open("a.txt", 6, "")},
		{"open at end", "rw", open("a.txt", 11, "")},
		{"open empty file", "rw", open("empty.txt", 0, "")},
		{"open past end", "rw", open("a.txt", 12, "")},
		{"open negative", "rw", open("a.txt", -1, "")},
		{"open etag ok", "rw", open("a.txt", 3, vfs.ETag(11, time.Unix(1700000000, 123456789)))},
		{"open etag stale", "rw", open("a.txt", 0, "1-1")},
		{"open missing", "rw", open("nope", 0, "")},
		{"open dir", "rw", open("sub", 0, "")},
		{"hash", "rw", hash("a.txt")},
		{"hash missing", "rw", hash("nope")},
		{"hash dir", "rw", hash("sub")},
		{"put new", "rw", put("new.txt", "data", vfs.PutOptions{ModTime: fixed})},
		{"put exists", "rw", put("a.txt", "data", vfs.PutOptions{})},
		{"put overwrite", "rw", put("a.txt", "data!", vfs.PutOptions{Overwrite: true, ModTime: fixed})},
		{"put hash ok", "rw", put("h.txt", "hello world", vfs.PutOptions{SHA256: sum, ModTime: fixed})},
		{"put hash bad", "rw", put("h.txt", "hello wurld", vfs.PutOptions{SHA256: sum})},
		{"put hash bad overwrite", "rw", put("a.txt", "x", vfs.PutOptions{Overwrite: true, SHA256: sum})},
		{"put missing parent", "rw", put("no/dir/f", "d", vfs.PutOptions{})},
		{"put traversal", "rw", put("../f", "d", vfs.PutOptions{})},
		{"put over dir", "rw", put("sub", "d", vfs.PutOptions{Overwrite: true})},
		{"put read-only", "ro", put("new.txt", "d", vfs.PutOptions{})},
		{"put disk full", "full", put("new.txt", "d", vfs.PutOptions{})},
		{"mkdir", "rw", other(func(f vfs.FS) error { return f.Mkdir(bg, "d1") }, "d1")},
		{"mkdir exists", "rw", other(func(f vfs.FS) error { return f.Mkdir(bg, "sub") }, "sub")},
		{"mkdir no parent", "rw", other(func(f vfs.FS) error { return f.Mkdir(bg, "x/y") }, "x")},
		{"mkdir read-only", "ro", other(func(f vfs.FS) error { return f.Mkdir(bg, "d1") }, "d1")},
		{"remove file", "rw", other(func(f vfs.FS) error { return f.Remove(bg, "a.txt", false) }, "a.txt")},
		{"remove not empty", "rw", other(func(f vfs.FS) error { return f.Remove(bg, "sub", false) }, "sub")},
		{"remove recursive", "rw", other(func(f vfs.FS) error { return f.Remove(bg, "sub", true) }, "sub")},
		{"remove missing", "rw", other(func(f vfs.FS) error { return f.Remove(bg, "nope", false) }, "nope")},
		{"remove root", "rw", other(func(f vfs.FS) error { return f.Remove(bg, "", true) }, "")},
		{"remove read-only", "ro", other(func(f vfs.FS) error { return f.Remove(bg, "a.txt", false) }, "a.txt")},
		{"rename", "rw", other(func(f vfs.FS) error { return f.Rename(bg, "a.txt", "z.txt") }, "z.txt")},
		{"rename onto existing", "rw", other(func(f vfs.FS) error { return f.Rename(bg, "a.txt", "empty.txt") }, "empty.txt")},
		{"rename missing", "rw", other(func(f vfs.FS) error { return f.Rename(bg, "nope", "z") }, "z")},
		{"rename into itself", "rw", other(func(f vfs.FS) error { return f.Rename(bg, "sub", "sub/in") }, "sub/in")},
		{"rename traversal", "rw", other(func(f vfs.FS) error { return f.Rename(bg, "a.txt", "../z") }, "a.txt")},
		{"rename read-only", "ro", other(func(f vfs.FS) error { return f.Rename(bg, "a.txt", "z") }, "a.txt")},
	}
}

func TestRemoteFSConformance(t *testing.T) {
	cases := convCases()
	diskFull := func(_ context.Context, _ string, r io.Reader, _ vfs.PutOptions) (vfs.Entry, error) {
		io.Copy(io.Discard, r)
		return vfs.Entry{}, vfs.E(vfs.CodeDiskFull, "no space left on device")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := start(t, "linux")
			build := func(name string) (direct vfs.FS) {
				ro := name == "ro"
				l, dir := localShare(t, ro)
				seed(t, dir)
				if name == "full" {
					return &hookFS{FS: l, put: diskFull}
				}
				return l
			}
			// The remote side serves one tree; the direct side is an identical twin.
			remoteFS, directFS := build(tc.share), build(tc.share)
			e.share(tc.share, remoteFS, tc.share == "ro")
			want := tc.run(directFS)
			got := tc.run(e.c.FS(tc.share))
			if got != want {
				t.Fatalf("remote differs from direct\n remote: %s\n direct: %s", got, want)
			}
		})
	}
}

func TestOpenRangeAndIfRange(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	write(t, filepath.Join(dir, "f"), "0123456789")
	e.share("s", l, false)
	f := e.c.FS("s")

	st, err := f.Stat(bg, "f")
	if err != nil {
		t.Fatal(err)
	}
	rc, ent, err := f.Open(bg, "f", 4, st.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "456789" {
		t.Fatalf("tail = %q", got)
	}
	if ent.ETag != st.ETag || ent.Size != 10 {
		t.Fatalf("entry = %+v", ent)
	}
	// Offset equal to size is how Fetch resumes a complete part: no error, no bytes.
	rc, ent, err = f.Open(bg, "f", 10, st.ETag)
	if err != nil {
		t.Fatalf("open at size: %v", err)
	}
	if got := readAll(t, rc); got != "" || ent.Size != 10 {
		t.Fatalf("at size: %q %+v", got, ent)
	}
	// The file changes: the old etag no longer matches.
	later := time.Now().Add(time.Hour)
	write(t, filepath.Join(dir, "f"), "0123456789abc")
	os.Chtimes(filepath.Join(dir, "f"), later, later)
	_, _, err = f.Open(bg, "f", 4, st.ETag)
	wantCode(t, err, vfs.CodeChanged)
	_, _, err = f.Open(bg, "f", 0, st.ETag)
	wantCode(t, err, vfs.CodeChanged)
}

func TestLargeDownloadAndUpload(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	e.share("s", l, false)
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<19) // 8 MiB, above the JSON body limit
	if err := os.WriteFile(filepath.Join(dir, "big"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	f := e.c.FS("s")
	rc, _, err := f.Open(bg, "big", 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != string(big[1000:]) {
		t.Fatal("download differs")
	}
	if _, err := f.Put(bg, "up", bytes.NewReader(big), vfs.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "up")); !bytes.Equal(got, big) {
		t.Fatal("upload differs")
	}
}

func TestPutOptionsSurviveWire(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	e.share("s", l, false)
	f := e.c.FS("s")
	mt := time.Unix(1500000000, 987654321)

	ent, err := f.Put(bg, "p", strings.NewReader("hello world"), vfs.PutOptions{ModTime: mt})
	if err != nil {
		t.Fatal(err)
	}
	if !ent.ModTime.Equal(mt) {
		t.Fatalf("mtime = %v", ent.ModTime)
	}
	fi, _ := os.Stat(filepath.Join(dir, "p"))
	if !fi.ModTime().Equal(mt) {
		t.Fatalf("on disk mtime = %v", fi.ModTime())
	}
	_, err = f.Put(bg, "p", strings.NewReader("other"), vfs.PutOptions{})
	wantCode(t, err, vfs.CodeExists)
	if _, err = f.Put(bg, "p", strings.NewReader("other"), vfs.PutOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "p")); string(b) != "other" {
		t.Fatalf("overwrite left %q", b)
	}
	_, err = f.Put(bg, "q", strings.NewReader("hello wurld"),
		vfs.PutOptions{SHA256: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"})
	wantCode(t, err, vfs.CodeHashMismatch)
	if _, err := os.Lstat(filepath.Join(dir, "q")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatching upload left a file: %v", err)
	}
	if _, err := f.Put(bg, "q", strings.NewReader("hello world"),
		vfs.PutOptions{SHA256: "B94D27B9934D3E08A52E52D7DA7DABFAC484EFE37A5380EE9088F7ACE2EFCDE9"}); err != nil {
		t.Fatalf("matching upload: %v", err)
	}
}

func TestPutSourceErrorSurfaces(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	e.share("s", l, false)
	r := io.MultiReader(strings.NewReader("partial"), errReader{vfs.E(vfs.CodeChanged, "source moved")})
	_, err := e.c.FS("s").Put(bg, "p", r, vfs.PutOptions{})
	wantCode(t, err, vfs.CodeChanged)
	if _, err := os.Lstat(filepath.Join(dir, "p")); err == nil {
		t.Fatal("partial upload committed")
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestInvalidNameOnDestination(t *testing.T) {
	e := start(t, "windows") // the server believes it runs on Windows
	l, dir := localShare(t, false)
	seed(t, dir)
	e.share("s", l, false)
	f := e.c.FS("s")

	for _, name := range []string{"CON", "a/NUL.txt", "bad:name", "trail.", "sp ", `back\slash`, "q?", "x/y*z"} {
		_, err := f.Put(bg, name, strings.NewReader("d"), vfs.PutOptions{})
		wantCode(t, err, vfs.CodeInvalidName)
		wantCode(t, f.Mkdir(bg, name), vfs.CodeInvalidName)
		wantCode(t, f.Rename(bg, "a.txt", name), vfs.CodeInvalidName)
	}
	// A big upload rejected before its body is read still reports the code.
	_, err := f.Put(bg, "CON", bytes.NewReader(make([]byte, 3<<20)), vfs.PutOptions{})
	wantCode(t, err, vfs.CodeInvalidName)

	if _, err := f.Put(bg, "ok.txt", strings.NewReader("d"), vfs.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.Mkdir(bg, "good"); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename(bg, "a.txt", "good/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	// Source side is not name-checked: it must be able to name what exists.
	wantCode(t, f.Remove(bg, "bad:name", false), vfs.CodeNotFound)
	for _, n := range []string{"CON", "bad:name", "trail."} {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("%s was created", n)
		}
	}

	// On Linux the same names are fine.
	e2 := start(t, "linux")
	l2, _ := localShare(t, false)
	e2.share("s", l2, false)
	if _, err := e2.c.FS("s").Put(bg, "CON", strings.NewReader("d"), vfs.PutOptions{}); err != nil {
		t.Fatalf("linux: %v", err)
	}
}

// ---- failure mapping ----

func TestOfflineMapsToPeerOffline(t *testing.T) {
	e := start(t, "linux")
	e.srv.Close()
	t0 := time.Now()
	_, err := e.c.Info(bg)
	wantCode(t, err, vfs.CodePeerOffline)
	if d := time.Since(t0); d > 7*time.Second {
		t.Fatalf("took %v", d)
	}
	_, err = e.c.FS("s").List(bg, "")
	wantCode(t, err, vfs.CodePeerOffline)
	_, _, err = e.c.FS("s").Open(bg, "f", 0, "")
	wantCode(t, err, vfs.CodePeerOffline)
}

func TestCanceledContextMapsToCanceled(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	write(t, filepath.Join(dir, "f"), "x")
	started := make(chan struct{})
	e.share("s", &hookFS{FS: l, hash: func(ctx context.Context, p string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}, false)
	ctx, cancel := context.WithCancel(bg)
	go func() { <-started; cancel() }()
	_, err := e.c.FS("s").Hash(ctx, "f")
	wantCode(t, err, vfs.CodeCanceled)

	cctx, ccancel := context.WithCancel(bg)
	ccancel()
	_, err = e.c.Info(cctx)
	wantCode(t, err, vfs.CodeCanceled)
}

type stallReader struct {
	ctx   context.Context
	first int
}

func (s *stallReader) Read(b []byte) (int, error) {
	if s.first > 0 {
		n := min(len(b), s.first)
		s.first -= n
		return n, nil
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}
func (s *stallReader) Close() error { return nil }

func TestStreamDropMapsToPeerOffline(t *testing.T) {
	e := start(t, "linux")
	l, _ := localShare(t, false)
	e.share("s", &hookFS{FS: l, open: func(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error) {
		return &stallReader{ctx: ctx, first: 32 << 10},
			vfs.Entry{Name: "big", Path: "big", Type: vfs.TypeFile, Size: 1 << 20, ETag: "x"}, nil
	}}, false)
	rc, ent, err := e.c.FS("s").Open(bg, "big", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if ent.Size != 1<<20 {
		t.Fatalf("entry %+v", ent)
	}
	n, err := rc.Read(make([]byte, 4096))
	if err != nil || n == 0 {
		t.Fatalf("first read: %d %v", n, err)
	}
	e.srv.Close() // the server drops the connection mid-download
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := rc.Read(make([]byte, 64<<10)); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		var ve *vfs.Error
		if !errors.As(err, &ve) || ve.Code != vfs.CodePeerOffline {
			t.Fatalf("read error = %#v, want *vfs.Error peer_offline", err)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			t.Fatal("drop surfaced as EOF")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("read never failed")
	}
}

func TestShortBodyIsNotEOF(t *testing.T) {
	// The file shrinks after the headers were sent: the client must not see
	// a clean EOF, or Fetch would hash a truncated file as complete.
	e := start(t, "linux")
	l, _ := localShare(t, false)
	e.share("s", &hookFS{FS: l, open: func(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error) {
		return io.NopCloser(strings.NewReader("only5")),
			vfs.Entry{Name: "f", Path: "f", Type: vfs.TypeFile, Size: 100, ETag: "x"}, nil
	}}, false)
	rc, _, err := e.c.FS("s").Open(bg, "f", 0, "")
	if err == nil { // the abort may land before or after the headers
		defer rc.Close()
		_, err = io.ReadAll(rc)
	}
	wantCode(t, err, vfs.CodePeerOffline)
}

func TestAddrFailover(t *testing.T) {
	e := start(t, "linux")
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	c := NewClient(e.cliID, e.srvID.ID, []string{deadAddr, e.addr})
	var mu sync.Mutex
	var tried []string
	c.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		mu.Lock()
		tried = append(tried, addr)
		mu.Unlock()
		return defaultDial(ctx, addr)
	}
	if _, err := c.Info(bg); err != nil {
		t.Fatal(err)
	}
	c.hc.CloseIdleConnections()
	if _, err := c.Info(bg); err != nil {
		t.Fatal(err)
	}
	want := []string{deadAddr, e.addr, e.addr}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(tried, want) {
		t.Fatalf("dial order = %v, want %v", tried, want)
	}
}

func TestAddrFailoverPastWrongIdentity(t *testing.T) {
	// A stale address now held by another host must not block the live one.
	e := start(t, "linux")
	stranger := start(t, "linux")
	c := NewClient(e.cliID, e.srvID.ID, []string{stranger.addr, e.addr})
	if _, err := c.Info(bg); err != nil {
		t.Fatal(err)
	}
	if n := stranger.b.hitCount(); n != 0 {
		t.Fatalf("stranger saw %d calls", n)
	}
}

func TestSetAddrs(t *testing.T) {
	e := start(t, "linux")
	c := NewClient(e.cliID, e.srvID.ID, nil)
	_, err := c.Info(bg)
	wantCode(t, err, vfs.CodePeerOffline)
	c.SetAddrs([]string{e.addr})
	if _, err := c.Info(bg); err != nil {
		t.Fatal(err)
	}
}

// ---- wire hygiene ----

func rawGet(t *testing.T, e *env, rawURL string) (int, []byte) {
	t.Helper()
	resp, err := e.c.hc.Get(e.c.base + rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestPathTraversalOverWire(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	write(t, filepath.Join(dir, "in"), "x")
	e.share("s", l, false)

	paths := []string{
		"../../etc/passwd", "..", "sub/../../x", "/etc/passwd",
		"%2e%2e/%2e%2e/etc/passwd", "%2E%2E%2Fetc%2Fpasswd", "..%2f..%2fetc%2fpasswd", ".%2e/x",
		vfs.PartDir + "/x", "%2esugar-it-part",
	}
	for _, ep := range []string{"entries", "stat", "file", "hash"} {
		for _, p := range paths {
			code, body := rawGet(t, e, "/v1/shares/s/"+ep+"?path="+p)
			var ve vfs.Error
			json.Unmarshal(body, &ve)
			if code >= 200 && code < 300 && ep != "hash" {
				t.Errorf("%s ?path=%s: status %d", ep, p, code)
			}
			if ve.Code != vfs.CodeInvalidPath {
				t.Errorf("%s ?path=%s: status %d body %s", ep, p, code, body)
			}
		}
	}
	// The same through the typed client, which must send the raw string.
	f := e.c.FS("s")
	_, err := f.List(bg, "../../etc/passwd")
	wantCode(t, err, vfs.CodeInvalidPath)
	_, _, err = f.Open(bg, "a/../../x", 0, "")
	wantCode(t, err, vfs.CodeInvalidPath)
	wantCode(t, f.Mkdir(bg, "../x"), vfs.CodeInvalidPath)
	wantCode(t, f.Remove(bg, "../x", true), vfs.CodeInvalidPath)
	wantCode(t, f.Rename(bg, "in", "../out"), vfs.CodeInvalidPath)
	wantCode(t, f.Rename(bg, "../in", "out"), vfs.CodeInvalidPath)
	// Share names cannot be used to climb either.
	_, err = e.c.FS("../s").List(bg, "")
	if vfs.CodeOf(err) != vfs.CodeShareNotFound && vfs.CodeOf(err) != vfs.CodeInvalidRequest {
		t.Fatalf("share traversal: %v", err)
	}
	_, err = e.c.FS("nope").List(bg, "")
	wantCode(t, err, vfs.CodeShareNotFound)
}

func TestSharesDoNotLeakLocalPath(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	ro, rodir := localShare(t, true)
	e.share("docs", l, false)
	e.share("ro", ro, true)

	code, body := rawGet(t, e, "/v1/shares")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, leak := range []string{dir, rodir, filepath.Dir(dir), "/tmp", os.TempDir()} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("response leaks %q: %s", leak, body)
		}
	}
	var generic []map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatal(err)
	}
	for _, m := range generic {
		if len(m) != 2 || m["Name"] == nil || m["ReadOnly"] == nil {
			t.Fatalf("unexpected fields: %v", m)
		}
	}
	got, err := e.c.Shares(bg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []ShareInfo{{"docs", false}, {"ro", true}}) {
		t.Fatalf("shares = %+v", got)
	}
	// Info is a pure data call.
	info, err := e.c.Info(bg)
	if err != nil || info.Protocol != ProtocolMajor || info.ID != "self-id" {
		t.Fatalf("info = %+v %v", info, err)
	}
}

func TestHashFlushesHeaders(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	write(t, filepath.Join(dir, "f"), "hello world")
	release := make(chan struct{})
	e.share("s", &hookFS{FS: l, hash: func(ctx context.Context, p string) (string, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return l.Hash(ctx, p)
	}}, false)

	got := make(chan *http.Response, 1)
	go func() {
		resp, err := e.c.hc.Get(e.c.base + "/v1/shares/s/hash?path=f")
		if err != nil {
			t.Error(err)
			got <- nil
			return
		}
		got <- resp
	}()
	var resp *http.Response
	select {
	case resp = <-got: // headers arrived while the hash is still pending
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("headers were not flushed before the hash finished")
	}
	if resp == nil {
		close(release)
		return
	}
	close(release)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9") {
		t.Fatalf("body = %s", b)
	}
}

func TestHashFailureAfterFlushKeepsItsCode(t *testing.T) {
	e := start(t, "linux")
	l, dir := localShare(t, false)
	write(t, filepath.Join(dir, "f"), "x")
	fail := func(err error) vfs.FS {
		return &hookFS{FS: l, hash: func(context.Context, string) (string, error) { return "", err }}
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{vfs.E(vfs.CodeChanged, "file changed while opening"), vfs.CodeChanged},
		{vfs.E(vfs.CodeNotFound, "gone"), vfs.CodeNotFound},
		{vfs.E(vfs.CodeDiskFull, "full"), vfs.CodeDiskFull},
		{errors.New("boom /secret/path"), vfs.CodeInternal},
	} {
		e.share("s", fail(tc.err), false)
		sum, err := e.c.FS("s").Hash(bg, "f")
		wantCode(t, err, tc.want)
		if sum != "" {
			t.Fatalf("sum = %q alongside error", sum)
		}
		if tc.want == vfs.CodeInternal && strings.Contains(err.Error(), "/secret/path") {
			t.Fatalf("internal error text leaked: %v", err)
		}
	}
	// The status line is 200 for all of them: the failure rides in the body.
	e.share("s", fail(vfs.E(vfs.CodeChanged, "x")), false)
	code, body := rawGet(t, e, "/v1/shares/s/hash?path=f")
	var ve vfs.Error
	json.Unmarshal(body, &ve)
	if code != 200 || ve.Code != vfs.CodeChanged {
		t.Fatalf("status %d body %s", code, body)
	}
	// Truncated body (server vanished mid-hash) is peer_offline, never a sum.
	block := &hookFS{FS: l, hash: func(ctx context.Context, p string) (string, error) { <-ctx.Done(); return "", ctx.Err() }}
	e.share("s", block, false)
	go func() { time.Sleep(200 * time.Millisecond); e.srv.Close() }()
	_, err := e.c.FS("s").Hash(bg, "f")
	wantCode(t, err, vfs.CodePeerOffline)
}

func TestJSONBodyLimitAppliesToJSONOnly(t *testing.T) {
	e := start(t, "linux")
	huge := `{"Token":"` + strings.Repeat("a", 5<<20) + `"}`
	for _, p := range []string{"/v1/pair", "/v1/members/sync", "/v1/jobs", "/v1/shares/s/mkdir", "/v1/shares/s/remove", "/v1/shares/s/rename"} {
		resp, err := e.c.hc.Post(e.c.base+p, "application/json", strings.NewReader(huge))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var ve vfs.Error
		json.Unmarshal(b, &ve)
		if resp.StatusCode != http.StatusRequestEntityTooLarge || ve.Code != vfs.CodeInvalidRequest {
			t.Errorf("%s: status %d body %s", p, resp.StatusCode, b)
		}
	}
	if n := e.b.hitCount(); n > 6 { // only the membership checks, no backend work
		t.Errorf("backend hit %d times", n)
	}
	// Malformed JSON is an invalid_request, not a 500.
	resp, _ := e.c.hc.Post(e.c.base+"/v1/members/sync", "application/json", strings.NewReader("{nope"))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var ve vfs.Error
	json.Unmarshal(b, &ve)
	if ve.Code != vfs.CodeInvalidRequest {
		t.Errorf("malformed: %d %s", resp.StatusCode, b)
	}
}

// ---- membership, jobs ----

func TestMembersAndJobsRoundTrip(t *testing.T) {
	e := start(t, "linux")
	in := []mesh.Member{{ID: "m1", Name: "one", Addrs: []string{"10.0.0.1:7443", "[fe80::1]:7443"}, Generation: 7},
		{ID: "m2", Name: "tomb", Generation: 8, Removed: true}}
	out, err := e.c.SyncMembers(bg, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[2].ID != "added" || !reflect.DeepEqual(out[:2], in) {
		t.Fatalf("out = %+v", out)
	}
	if !reflect.DeepEqual(e.b.syncIn, in) || e.b.syncCaller != e.cliID.ID {
		t.Fatalf("server saw %+v from %q", e.b.syncIn, e.b.syncCaller)
	}

	spec := transfer.Spec{Op: "move", Src: transfer.Endpoint{Peer: "p1", Share: "a", Path: "x/ü y"},
		Dst: transfer.Endpoint{Peer: "p2", Share: "b", Path: ""}, OnConflict: transfer.ConflictRename}
	j, err := e.c.SubmitJob(bg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if j.ID != "j1" || j.Origin != e.cliID.ID || !reflect.DeepEqual(j.Spec, spec) || j.State != transfer.StateQueued {
		t.Fatalf("job = %+v", j)
	}
	jobs, err := e.c.ListJobs(bg, e.cliID.ID)
	if err != nil || len(jobs) != 1 || !reflect.DeepEqual(jobs[0], j) {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if jobs, _ := e.c.ListJobs(bg, "someone-else"); len(jobs) != 0 {
		t.Fatalf("origin filter ignored: %+v", jobs)
	}
	if err := e.c.CancelJob(bg, "j1"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.RetryJob(bg, "j1"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.c.CancelJob(bg, "nope"), vfs.CodeNotFound)
	wantCode(t, e.c.RetryJob(bg, "nope"), vfs.CodeNotFound)
	if err := e.c.CancelJob(bg, "a b/c"); err != nil { // ids are path-escaped
		t.Fatalf("escaped id: %v", err)
	}
}

// ---- review round 1 ----

func TestNonMemberGets403OnEveryNonPairPath(t *testing.T) {
	e := start(t, "linux")
	e.b.setMember(e.cliID.ID, false)
	paths := []string{
		"/v1%2Fpair", "/v1/pai%72", "/v1/pair/", "/v1//pair", "/v1/pair/../shares", "/v1/pair/%2e%2e/shares",
		"/v1%2Fshares", "/v1/shares", "/v1/info", "/nope", "/", "/v1/PAIR", "/v1/pair%2F",
	}
	for _, m := range []string{"GET", "POST", "PUT"} {
		for _, p := range paths {
			req, err := http.NewRequest(m, e.c.base+p, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := e.c.hc.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, p, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var ve vfs.Error
			json.Unmarshal(b, &ve)
			if resp.StatusCode != 403 || ve.Code != vfs.CodePeerUnauthorized {
				t.Errorf("%s %s: status %d body %s", m, p, resp.StatusCode, b)
			}
		}
	}
	if e.b.pairCaller != "" {
		t.Fatal("a non-pair path reached Pair")
	}
}

func TestFilterWriterKeepsRealErrors(t *testing.T) {
	var out syncBuf
	l := log.New(&filterWriter{w: &out}, "", 0)
	l.Printf("http: TLS handshake error from 1.2.3.4:5: EOF")
	l.Printf("http: panic serving 1.2.3.4:5: boom")
	if got := out.String(); strings.Contains(got, "handshake") || !strings.Contains(got, "panic serving") {
		t.Fatalf("log = %q", got)
	}
}

func TestHandlerPanicIsLogged(t *testing.T) {
	e := start(t, "linux")
	l, _ := localShare(t, false)
	e.share("s", &hookFS{FS: l, hash: func(context.Context, string) (string, error) { panic("fs bug") }}, false)
	e.c.FS("s").Hash(bg, "f")
	_, err := e.c.FS("s").Stat(bg, "") // handshake-noise path stays quiet too
	_ = err
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(e.logs.String(), "fs bug") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(e.logs.String(), "fs bug") {
		t.Fatalf("panic not logged: %q", e.logs.String())
	}
}

func TestJSONBodyReadDeadline(t *testing.T) {
	old := jsonReadTimeout
	jsonReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { jsonReadTimeout = old })
	e := start(t, "linux")
	e.b.setMember(e.cliID.ID, false) // a stranger, who may only pair
	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write([]byte("{")) // then stalls forever
	req, _ := http.NewRequest("POST", e.c.base+"/v1/pair", pr)
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := e.c.hc.Do(req)
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	select {
	case resp := <-done:
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 400 || !strings.Contains(string(b), vfs.CodeInvalidRequest) {
				t.Fatalf("status %d body %s", resp.StatusCode, b)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled JSON body held the handler open")
	}
}

func TestUploadHasNoReadDeadline(t *testing.T) {
	old := jsonReadTimeout
	jsonReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { jsonReadTimeout = old })
	e := start(t, "linux")
	l, dir := localShare(t, false)
	e.share("s", l, false)
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("slow"))
		time.Sleep(700 * time.Millisecond)
		pw.Write([]byte("er"))
		pw.Close()
	}()
	if _, err := e.c.FS("s").Put(bg, "f", pr, vfs.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "slower" {
		t.Fatalf("got %q", b)
	}
}

type ctxFailReader struct{ ctx context.Context }

func (r ctxFailReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, vfs.E(vfs.CodeChanged, "source gone")
}

func TestPutCancelRacesWithSourceError(t *testing.T) {
	e := start(t, "linux")
	l, _ := localShare(t, false)
	e.share("s", l, false)
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithCancel(bg)
		time.AfterFunc(5*time.Millisecond, cancel)
		_, err := e.c.FS("s").Put(ctx, "f", ctxFailReader{ctx}, vfs.PutOptions{})
		wantCode(t, err, vfs.CodeCanceled)
		cancel()
	}
}
