package node

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/identity"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

var bg = context.Background()

// ---- fixtures ----

// tnode is a node with a real TLS server on a loopback port.
type tnode struct {
	t      *testing.T
	n      *Node
	id     *identity.Identity
	cfg    *config.Store
	dir    string
	addr   string
	srv    *http.Server
	cancel context.CancelFunc
	done   chan struct{}
	skew   atomic.Int64 // added to the node's clock, in nanoseconds
}

type bootOpts struct {
	listen string                                // default 127.0.0.1:0
	wrap   func(peerapi.Backend) peerapi.Backend // test double for the backend
	idle   bool                                  // do not run the gossip loops
}

func boot(t *testing.T, dir, name string, o bootOpts) *tnode {
	t.Helper()
	id, err := identity.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if o.listen == "" {
		o.listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := cfg.Update(func(c *config.Config) error {
		c.Name, c.AdvertiseAddrs = name, []string{addr}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	n, err := New(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	n.gossipEvery, n.healthEvery, n.callTimeout = 100*time.Millisecond, 100*time.Millisecond, 3*time.Second
	tn := &tnode{t: t, n: n, id: id, cfg: cfg, dir: dir, addr: addr}
	n.now = func() time.Time { return time.Now().Add(time.Duration(tn.skew.Load())) }
	var b peerapi.Backend = n.Backend()
	if o.wrap != nil {
		b = o.wrap(b)
	}
	tn.srv = peerapi.NewServer(id, b)
	go tn.srv.ServeTLS(ln, "", "")
	t.Cleanup(tn.stop)
	if !o.idle {
		tn.run()
	}
	return tn
}

func startNode(t *testing.T, name string) *tnode { return boot(t, t.TempDir(), name, bootOpts{}) }

func (tn *tnode) run() {
	ctx, cancel := context.WithCancel(bg)
	tn.cancel, tn.done = cancel, make(chan struct{})
	go func() { defer close(tn.done); tn.n.Run(ctx) }()
}

func (tn *tnode) stop() {
	if tn.cancel != nil {
		tn.cancel()
		select {
		case <-tn.done:
		case <-time.After(5 * time.Second):
			tn.t.Error("Run did not stop after cancel")
		}
		tn.cancel = nil
	}
	// Shutdown waits for in-flight handlers, which may still write the config
	// that TempDir cleanup is about to remove.
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	if tn.srv.Shutdown(ctx) != nil {
		tn.srv.Close()
	}
}

func (tn *tnode) share(name string, files map[string]string) string {
	tn.t.Helper()
	dir := tn.t.TempDir()
	for p, s := range files {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			tn.t.Fatal(err)
		}
	}
	if err := tn.n.AddShare(shares.Spec{Name: name, Path: dir}); err != nil {
		tn.t.Fatal(err)
	}
	return dir
}

func pair(t *testing.T, inviter, joiner *tnode) {
	t.Helper()
	inv, err := inviter.n.Invite(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := joiner.n.Join(bg, inv); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func peer(n *Node, id string) (PeerStatus, bool) {
	for _, p := range n.Peers() {
		if p.ID == id {
			return p, true
		}
	}
	return PeerStatus{}, false
}

func sees(n *Node, id string) bool { _, ok := peer(n, id); return ok }

func seesOnline(n *Node, id string) bool { p, ok := peer(n, id); return ok && p.Online }

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := vfs.CodeOf(err); got != code {
		t.Fatalf("code = %q (%v), want %q", got, err, code)
	}
}

func member(t *testing.T, tn *tnode, id string) (mesh.Member, bool) {
	t.Helper()
	for _, m := range tn.cfg.Get().Members {
		if m.ID == id {
			return m, true
		}
	}
	return mesh.Member{}, false
}

// ---- pairing ----

func TestPairTwoNodes(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	inv, err := a.n.Invite(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := b.n.Join(bg, inv)
	if err != nil {
		t.Fatal(err)
	}
	if ps.ID != a.id.ID || ps.Name != "alpha" {
		t.Fatalf("%+v", ps)
	}
	eventually(t, "both online", func() bool { return seesOnline(a.n, b.id.ID) && seesOnline(b.n, a.id.ID) })
	if p, _ := peer(b.n, a.id.ID); p.Name != "alpha" || !slices.Equal(p.Addrs, []string{a.addr}) || p.LastSeen.IsZero() {
		t.Fatalf("%+v", p)
	}
}

func TestInviteOneTimeAndExpiry(t *testing.T) {
	a, b, c := startNode(t, "alpha"), startNode(t, "bravo"), startNode(t, "charlie")
	inv, _ := a.n.Invite(time.Minute)
	if _, err := b.n.Join(bg, inv); err != nil {
		t.Fatal(err)
	}
	_, err := c.n.Join(bg, inv)
	wantCode(t, err, vfs.CodePeerUnauthorized)
	if sees(c.n, a.id.ID) || sees(a.n, c.id.ID) {
		t.Fatal("a rejected join added a member")
	}

	// The inviter's clock runs past the TTL: the server refuses the token.
	late, _ := a.n.Invite(time.Minute)
	a.skew.Store(int64(2 * time.Minute))
	_, err = c.n.Join(bg, late)
	wantCode(t, err, vfs.CodePeerUnauthorized)
	a.skew.Store(0)

	// An invite that is already expired is refused without contacting anyone
	// (the address would answer peer_offline).
	dead := mesh.Invite{ID: a.id.ID, Name: "alpha", Addrs: []string{"127.0.0.1:1"}, Token: "tok", Expires: time.Now().Add(-time.Second)}
	_, err = c.n.Join(bg, dead.Encode())
	wantCode(t, err, vfs.CodePeerUnauthorized)
}

func TestJoinRejectsMalformedInvites(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	good := mesh.Invite{ID: a.id.ID, Name: "alpha", Addrs: []string{a.addr}, Token: "tok", Expires: time.Now().Add(time.Minute)}
	for name, mut := range map[string]func(*mesh.Invite){
		"bad name":  func(i *mesh.Invite) { i.Name = "Not Valid" },
		"reserved":  func(i *mesh.Invite) { i.Name = "local" },
		"self":      func(i *mesh.Invite) { i.ID = b.id.ID },
		"not an id": func(i *mesh.Invite) { i.ID = "../x" },
	} {
		inv := good
		mut(&inv)
		_, err := b.n.Join(bg, inv.Encode())
		if vfs.CodeOf(err) != vfs.CodeInvalidRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := b.n.Join(bg, "garbage")
	wantCode(t, err, vfs.CodeInvalidRequest)
}

func TestInviteWrongFingerprint(t *testing.T) {
	a, b, c := startNode(t, "alpha"), startNode(t, "bravo"), startNode(t, "charlie")
	inv, _ := a.n.Invite(time.Minute)
	forged, _ := mesh.DecodeInvite(inv)
	forged.ID = c.id.ID // a different, real host
	_, err := b.n.Join(bg, forged.Encode())
	if err == nil {
		t.Fatal("join with a mismatched ID succeeded")
	}
	for _, tn := range []*tnode{a, b, c} {
		if len(tn.n.Peers()) != 0 {
			t.Fatalf("%s gained members after a failed join", tn.cfg.Get().Name)
		}
	}
	// The pin failed before anything was sent, so the token is still good.
	if _, err := b.n.Join(bg, inv); err != nil {
		t.Fatalf("token was consumed by the failed join: %v", err)
	}
}

// ---- membership ----

func TestMeshPropagates(t *testing.T) {
	a, b, c := startNode(t, "alpha"), startNode(t, "bravo"), startNode(t, "charlie")
	c.share("docs", map[string]string{"hello.txt": "hi"})
	pair(t, a, b)
	pair(t, b, c)
	eventually(t, "a and c see each other", func() bool { return seesOnline(a.n, c.id.ID) && seesOnline(c.n, a.id.ID) })
	es, err := mustFS(t, a.n, "charlie", "docs").List(bg, "")
	if err != nil || len(es) != 1 || es[0].Name != "hello.txt" {
		t.Fatalf("%v %v", es, err)
	}
}

func mustFS(t *testing.T, n *Node, peer, share string) vfs.FS {
	t.Helper()
	f, err := n.FS(bg, peer, share)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func trio(t *testing.T) (a, b, c *tnode) {
	a, b, c = startNode(t, "alpha"), startNode(t, "bravo"), startNode(t, "charlie")
	pair(t, a, b)
	pair(t, a, c)
	eventually(t, "full mesh", func() bool {
		return seesOnline(a.n, b.id.ID) && seesOnline(a.n, c.id.ID) && seesOnline(b.n, c.id.ID) &&
			seesOnline(c.n, b.id.ID) && seesOnline(b.n, a.id.ID) && seesOnline(c.n, a.id.ID)
	})
	return
}

func TestRemovePropagatesAndBlocks(t *testing.T) {
	a, b, c := trio(t)
	if err := a.n.RemovePeer("charlie"); err != nil {
		t.Fatal(err)
	}
	if sees(a.n, c.id.ID) {
		t.Fatal("still listed on the remover")
	}
	eventually(t, "b drops c", func() bool { return !sees(b.n, c.id.ID) })
	for _, srv := range []*tnode{a, b} {
		cl := peerapi.NewClient(c.id, srv.id.ID, []string{srv.addr})
		_, err := cl.Shares(bg)
		wantCode(t, err, vfs.CodePeerUnauthorized)
	}
	wantCode(t, a.n.RemovePeer("nosuch"), vfs.CodePeerNotFound)
	wantCode(t, a.n.RemovePeer("local"), vfs.CodeInvalidRequest)
	wantCode(t, a.n.RemovePeer("alpha"), vfs.CodeInvalidRequest)
}

func TestRemovedCannotReviveByGossip(t *testing.T) {
	a, c, d := startNode(t, "alpha"), startNode(t, "charlie"), boot(t, t.TempDir(), "delta", bootOpts{idle: true})
	pair(t, a, c)
	pair(t, a, d) // d now holds c as active, and never gossips again
	stale := d.cfg.Get().Members
	if m, ok := find(stale, c.id.ID); !ok || m.Removed {
		t.Fatalf("d does not know c: %+v", stale)
	}
	if err := a.n.RemovePeer("charlie"); err != nil {
		t.Fatal(err)
	}
	cl := peerapi.NewClient(d.id, a.id.ID, []string{a.addr})
	if _, err := cl.SyncMembers(bg, stale); err != nil {
		t.Fatal(err)
	}
	if sees(a.n, c.id.ID) {
		t.Fatal("a stale list revived a removed member")
	}
	if m, _ := member(t, a, c.id.ID); !m.Removed {
		t.Fatalf("tombstone lost: %+v", m)
	}
	_, err := a.n.Client(c.id.ID)
	wantCode(t, err, vfs.CodePeerNotFound)
}

func find(ms []mesh.Member, id string) (mesh.Member, bool) {
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return mesh.Member{}, false
}

func TestRejoinAfterRemove(t *testing.T) {
	a, b, c := trio(t)
	a.share("docs", map[string]string{"x": "1"})
	if err := a.n.RemovePeer("charlie"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b drops c", func() bool { return !sees(b.n, c.id.ID) })
	if _, err := c.n.Shares(bg, "alpha"); err == nil {
		t.Fatal("removed host still reads shares")
	}
	pair(t, a, c)
	eventually(t, "c is back everywhere", func() bool { return seesOnline(a.n, c.id.ID) && seesOnline(b.n, c.id.ID) })
	infos, err := c.n.Shares(bg, "alpha")
	if err != nil || len(infos) != 1 || infos[0].Name != "docs" {
		t.Fatalf("%v %v", infos, err)
	}
}

// R7: a rejoin raises this host's record on the others above its own, yet its
// own later changes must still win everywhere.
func TestOwnChangesPropagateAfterRejoin(t *testing.T) {
	a, b, c := trio(t)
	if err := a.n.RemovePeer("charlie"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b drops c", func() bool { return !sees(b.n, c.id.ID) })
	pair(t, a, c)
	eventually(t, "c is back", func() bool { return seesOnline(a.n, c.id.ID) && seesOnline(b.n, c.id.ID) })
	onA, _ := member(t, a, c.id.ID)
	own, _ := member(t, c, c.id.ID)
	if onA.Generation <= own.Generation {
		t.Fatalf("setup: record on a (%d) should be above c's own (%d)", onA.Generation, own.Generation)
	}

	if err := c.n.RenameSelf("charlie2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rename reaches a and b", func() bool {
		pa, _ := peer(a.n, c.id.ID)
		pb, _ := peer(b.n, c.id.ID)
		return pa.Name == "charlie2" && pb.Name == "charlie2"
	})

	// An address change goes the same way. Raise the record on others first.
	if err := a.n.RemovePeer("charlie2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b drops c again", func() bool { return !sees(b.n, c.id.ID) })
	pair(t, a, c)
	eventually(t, "c is back again", func() bool { return seesOnline(a.n, c.id.ID) && seesOnline(b.n, c.id.ID) })
	if err := c.cfg.Update(func(cf *config.Config) error {
		cf.AdvertiseAddrs = []string{c.addr, "127.0.0.1:9"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := c.n.refreshSelf(); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	eventually(t, "new address reaches a and b", func() bool {
		pa, _ := peer(a.n, c.id.ID)
		pb, _ := peer(b.n, c.id.ID)
		return slices.Contains(pa.Addrs, "127.0.0.1:9") && slices.Contains(pb.Addrs, "127.0.0.1:9")
	})
}

func TestAddrChangePropagates(t *testing.T) {
	a := startNode(t, "alpha")
	dir := t.TempDir()
	b := boot(t, dir, "bravo", bootOpts{})
	pair(t, a, b)
	eventually(t, "online", func() bool { return seesOnline(a.n, b.id.ID) })
	before, _ := member(t, a, b.id.ID)
	oldAddr := b.addr
	b.stop()
	eventually(t, "offline", func() bool { p, _ := peer(a.n, b.id.ID); return !p.Online })

	b2 := boot(t, dir, "bravo", bootOpts{}) // same identity, new port
	if b2.addr == oldAddr {
		t.Skip("got the same port again")
	}
	eventually(t, "a reaches the new address", func() bool {
		p, _ := peer(a.n, b.id.ID)
		return p.Online && slices.Equal(p.Addrs, []string{b2.addr})
	})
	after, _ := member(t, a, b.id.ID)
	if after.Generation <= before.Generation {
		t.Fatalf("generation %d -> %d", before.Generation, after.Generation)
	}
}

func TestDetectedAddrsUsedWithoutAdvertise(t *testing.T) {
	dir := t.TempDir()
	id, _ := identity.LoadOrCreate(dir)
	cfg, _ := config.Open(dir)
	cfg.Update(func(c *config.Config) error { c.ListenAddr = ":7443"; c.AdvertiseAddrs = nil; return nil })
	n, err := New(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	n.ifaces = func() ([]mesh.Iface, error) {
		return []mesh.Iface{
			{Name: "docker0", IPs: []netip.Addr{netip.MustParseAddr("172.17.0.1")}},
			{Name: "eth0", IPs: []netip.Addr{netip.MustParseAddr("192.168.0.5")}},
		}, nil
	}
	if changed, err := n.refreshSelf(); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	inv, err := n.Invite(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := mesh.DecodeInvite(inv)
	if !slices.Equal(d.Addrs, []string{"192.168.0.5:7443"}) {
		t.Fatalf("%v", d.Addrs)
	}
}

func TestIncomingMembersAreSanitized(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	pair(t, a, b)
	selfBefore, _ := member(t, a, a.id.ID)
	evil := []mesh.Member{
		{ID: "", Name: "nameless", Generation: 1},
		{ID: "badname", Name: "Bad_Name", Generation: 1},
		{ID: "reserved", Name: "local", Generation: 1},
		{ID: a.id.ID, Name: "evil", Addrs: []string{"6.6.6.6:1"}, Generation: 99, Removed: true},
		{ID: "good", Name: "good", Addrs: []string{"10.0.0.1:7443"}, Generation: 1},
	}
	out, err := a.n.Backend().SyncMembers(bg, b.id.ID, evil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range append(out, a.cfg.Get().Members...) {
		if m.ID == "" || !mesh.ValidName(m.Name) {
			t.Fatalf("invalid member kept: %+v", m)
		}
	}
	if sees(a.n, "badname") || sees(a.n, "reserved") || !sees(a.n, "good") {
		t.Fatalf("%+v", a.n.Peers())
	}
	selfAfter, _ := member(t, a, a.id.ID)
	if selfAfter.Name != selfBefore.Name || selfAfter.Removed || !slices.Equal(selfAfter.Addrs, selfBefore.Addrs) {
		t.Fatalf("self changed: %+v -> %+v", selfBefore, selfAfter)
	}
	if got, _, err := a.n.ResolvePeer("alpha"); err != nil || got != a.id.ID {
		t.Fatalf("%v %v", got, err)
	}
}

func TestRenameSelfPropagates(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	pair(t, a, b)
	eventually(t, "online", func() bool { return seesOnline(b.n, a.id.ID) })
	if err := a.n.RenameSelf("alpha2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b sees the new name", func() bool { p, _ := peer(b.n, a.id.ID); return p.Name == "alpha2" })
	if _, local, err := b.n.ResolvePeer("alpha2"); err != nil || local {
		t.Fatalf("%v %v", local, err)
	}
	for _, bad := range []string{"", "Alpha", "local", "a_b", "has space", "한글"} {
		wantCode(t, a.n.RenameSelf(bad), vfs.CodeInvalidRequest)
	}
	if a.cfg.Get().Name != "alpha2" {
		t.Fatal("name changed by a rejected rename")
	}
}

// ---- shares ----

func TestAddShareOverlapRejected(t *testing.T) {
	a := startNode(t, "alpha")
	dir := a.share("docs", nil)
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	before := a.cfg.Get()
	for _, p := range []string{dir, sub, filepath.Dir(dir)} {
		wantCode(t, a.n.AddShare(shares.Spec{Name: "other", Path: p}), vfs.CodeShareOverlap)
	}
	if !equalSpecs(a.cfg.Get().Shares, before.Shares) {
		t.Fatal("config changed")
	}
	wantCode(t, a.n.AddShare(shares.Spec{Name: "docs", Path: t.TempDir()}), vfs.CodeExists)
	wantCode(t, a.n.AddShare(shares.Spec{Name: "Bad Name", Path: t.TempDir()}), vfs.CodeInvalidRequest)
}

func equalSpecs(a, b []shares.Spec) bool { return slices.Equal(a, b) }

func TestAddShareMissingPath(t *testing.T) {
	a := startNode(t, "alpha")
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o644)
	for _, p := range []string{filepath.Join(t.TempDir(), "nope"), file, ""} {
		wantCode(t, a.n.AddShare(shares.Spec{Name: "x", Path: p}), vfs.CodeInvalidRequest)
	}
	if len(a.cfg.Get().Shares) != 0 {
		t.Fatal("config changed")
	}
}

func TestRemoveShareClosesCachedFS(t *testing.T) {
	a := startNode(t, "alpha")
	a.share("docs", map[string]string{"f": "x"})
	f1, err := a.n.FS(bg, "local", "docs")
	if err != nil {
		t.Fatal(err)
	}
	f2, _ := a.n.FS(bg, "local", "docs")
	if f1 != f2 {
		t.Fatal("share reopened on every access")
	}
	if _, err := f1.List(bg, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.n.RemoveShare("docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := f1.List(bg, ""); err == nil {
		t.Fatal("cached filesystem of a removed share is still open")
	}
	_, err = a.n.FS(bg, "local", "docs")
	wantCode(t, err, vfs.CodeShareNotFound)
	wantCode(t, a.n.RemoveShare("docs"), vfs.CodeShareNotFound)
}

func TestMissingShareDirReportsError(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	pair(t, a, b)
	dir := a.share("docs", map[string]string{"f": "x"})
	moved := dir + ".gone"
	list := func(n *Node, peer string) error {
		f, err := n.FS(bg, peer, "docs")
		if err != nil {
			return err
		}
		_, err = f.List(bg, "")
		return err
	}
	if err := list(a.n, "local"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	wantCode(t, list(a.n, "local"), vfs.CodeNotFound)
	wantCode(t, list(b.n, "alpha"), vfs.CodeNotFound)
	if len(a.n.Peers()) != 1 {
		t.Fatal("node broken by a missing share dir")
	}
	a.n.fsMu.Lock()
	cached := len(a.n.fsCache)
	a.n.fsMu.Unlock()
	if cached != 0 {
		t.Fatal("a failed open was cached")
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	if err := list(a.n, "local"); err != nil {
		t.Fatalf("share did not reopen: %v", err)
	}
	// A replaced directory is a different directory: the cache must not keep
	// serving the old one.
	os.RemoveAll(dir)
	os.Mkdir(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "fresh"), nil, 0o644)
	es, err := mustFS(t, a.n, "local", "docs").List(bg, "")
	if err != nil || len(es) != 1 || es[0].Name != "fresh" {
		t.Fatalf("%v %v", es, err)
	}
}

// ---- routing ----

func TestLocalAlias(t *testing.T) {
	a := startNode(t, "alpha")
	for _, name := range []string{"local", "alpha"} {
		id, local, err := a.n.ResolvePeer(name)
		if err != nil || !local || id != a.id.ID {
			t.Fatalf("%s: %q %v %v", name, id, local, err)
		}
	}
	_, _, err := a.n.ResolvePeer("nosuch")
	wantCode(t, err, vfs.CodePeerNotFound)
}

func TestSameNamesGetAliases(t *testing.T) {
	a, b, c := startNode(t, "nas"), startNode(t, "nas"), startNode(t, "nas")
	pair(t, a, b)
	pair(t, a, c)
	eventually(t, "all seen", func() bool { return len(a.n.Peers()) == 2 })
	names := map[string]bool{}
	for _, p := range a.n.Peers() {
		id, _, err := a.n.ResolvePeer(p.Name)
		if err != nil || id != p.ID {
			t.Fatalf("%s -> %q %v", p.Name, id, err)
		}
		names[p.Name] = true
	}
	if len(names) != 2 {
		t.Fatalf("%v", names)
	}
}

func TestFSUnknownPeerAndShare(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	pair(t, a, b)
	b.share("docs", nil)
	_, err := a.n.FS(bg, "nosuch", "docs")
	wantCode(t, err, vfs.CodePeerNotFound)
	_, err = a.n.FS(bg, "bravo", "nosuch")
	wantCode(t, err, vfs.CodeShareNotFound)
	_, err = a.n.FS(bg, "local", "nosuch")
	wantCode(t, err, vfs.CodeShareNotFound)
	_, err = a.n.Shares(bg, "nosuch")
	wantCode(t, err, vfs.CodePeerNotFound)
	_, err = a.n.Client("nosuch")
	wantCode(t, err, vfs.CodePeerNotFound)
	if _, err := a.n.FS(bg, "bravo", "docs"); err != nil {
		t.Fatal(err)
	}
	ro, err := a.n.SourceReadOnly(bg, b.id.ID, "docs")
	if err != nil || ro {
		t.Fatalf("%v %v", ro, err)
	}
	_, err = a.n.SourceReadOnly(bg, b.id.ID, "nosuch")
	wantCode(t, err, vfs.CodeShareNotFound)
}

type protoV2 struct{ peerapi.Backend }

func (p protoV2) Info() peerapi.Info {
	i := p.Backend.Info()
	i.Protocol = peerapi.ProtocolMajor + 1
	return i
}

func TestProtocolMismatchMarked(t *testing.T) {
	f := boot(t, t.TempDir(), "future", bootOpts{wrap: func(b peerapi.Backend) peerapi.Backend { return protoV2{b} }})
	a := startNode(t, "alpha")
	pair(t, f, a)
	eventually(t, "marked incompatible", func() bool { p, ok := peer(a.n, f.id.ID); return ok && p.Incompatible })
	if p, _ := peer(a.n, f.id.ID); p.Online {
		t.Fatalf("incompatible peer is online: %+v", p)
	}
}

// ---- peerapi.Backend ----

func TestBackendJobs(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	pair(t, a, b)
	a.share("docs", map[string]string{"f": "x"})
	a.share("inbox", nil)
	be := a.n.Backend()
	spec := transfer.Spec{Op: "copy", Src: transfer.Endpoint{Peer: a.id.ID, Share: "docs", Path: "f"}, Dst: transfer.Endpoint{Peer: a.id.ID, Share: "inbox"}}

	_, err := be.SubmitJob(b.id.ID, spec)
	wantCode(t, err, vfs.CodeInternal) // before AttachJobs
	wantCode(t, be.CancelJob("x"), vfs.CodeInternal)
	wantCode(t, be.RetryJob("x"), vfs.CodeInternal)
	if len(be.ListJobs("")) != 0 {
		t.Fatal("jobs before AttachJobs")
	}

	m, err := transfer.NewManager(transfer.Options{Dir: t.TempDir(), SelfID: a.id.ID, Source: a.n.SourceFS, SourceReadOnly: a.n.SourceReadOnly, Dest: a.n.DestFS})
	if err != nil {
		t.Fatal(err)
	}
	a.n.AttachJobs(m)

	j, err := be.SubmitJob(b.id.ID, spec)
	if err != nil {
		t.Fatal(err)
	}
	if j.Origin != b.id.ID {
		t.Fatalf("origin %q, want the caller", j.Origin)
	}
	if got := be.ListJobs(b.id.ID); len(got) != 1 || got[0].ID != j.ID {
		t.Fatalf("%+v", got)
	}

	elsewhere := spec
	elsewhere.Dst.Peer = b.id.ID
	_, err = be.SubmitJob(b.id.ID, elsewhere)
	wantCode(t, err, vfs.CodeInvalidRequest)
	empty := spec
	empty.Dst.Peer = ""
	_, err = be.SubmitJob(b.id.ID, empty)
	wantCode(t, err, vfs.CodeInvalidRequest)
	stranger := spec
	stranger.Src.Peer = "someone-else"
	_, err = be.SubmitJob(b.id.ID, stranger)
	wantCode(t, err, vfs.CodeInvalidRequest)
	if err := be.CancelJob(j.ID); err != nil {
		t.Fatal(err)
	}
}

func TestBackendIsMemberAndInfo(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	be := a.n.Backend()
	if be.IsMember(b.id.ID) {
		t.Fatal("stranger is a member")
	}
	pair(t, a, b)
	if !be.IsMember(b.id.ID) || be.IsMember("") {
		t.Fatal("membership wrong")
	}
	if i := be.Info(); i.ID != a.id.ID || i.Name != "alpha" || i.Protocol != peerapi.ProtocolMajor {
		t.Fatalf("%+v", i)
	}
	a.share("docs", nil)
	if s := be.Shares(); len(s) != 1 || s[0].Name != "docs" {
		t.Fatalf("%+v", s)
	}
	if _, err := be.ShareFS("nosuch"); vfs.CodeOf(err) != vfs.CodeShareNotFound {
		t.Fatal(err)
	}
	if _, err := be.Pair(bg, b.id.ID, peerapi.PairRequest{Token: "bogus", Name: "bravo"}); vfs.CodeOf(err) != vfs.CodePeerUnauthorized {
		t.Fatal(err)
	}
}

// ---- lifecycle ----

func TestRunStopsAndRestarts(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "bravo")
	pair(t, a, b)
	b.stop() // a keeps trying an unreachable peer
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 3; i++ {
		a.stop()
		a.srv = &http.Server{} // stop() closes the server; only Run is restarted here
		a.run()
		time.Sleep(100 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := a.n.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
}

// R7 at the unit level: peers hold a higher generation of this host's record
// with the same content; the next own change must still go above it.
func TestOwnBumpUsesHighestGenerationSeen(t *testing.T) {
	a := boot(t, t.TempDir(), "alpha", bootOpts{idle: true})
	b := startNode(t, "bravo")
	pair(t, a, b)
	own, _ := member(t, a, a.id.ID)
	same := own
	same.Generation = own.Generation + 10
	if _, err := a.n.Backend().SyncMembers(bg, b.id.ID, []mesh.Member{same}); err != nil {
		t.Fatal(err)
	}
	if after, _ := member(t, a, a.id.ID); after.Generation != own.Generation {
		t.Fatalf("an identical remote record changed ours: %+v", after)
	}
	if err := a.n.RenameSelf("alpha2"); err != nil {
		t.Fatal(err)
	}
	if after, _ := member(t, a, a.id.ID); after.Generation != same.Generation+1 {
		t.Fatalf("generation %d, want %d", after.Generation, same.Generation+1)
	}
}

// A peer holds an older life of this host (different content) at a higher
// generation, for instance right after a restart: ours must supersede it.
func TestStaleRecordOfSelfIsSuperseded(t *testing.T) {
	a := boot(t, t.TempDir(), "alpha", bootOpts{idle: true})
	b := startNode(t, "bravo")
	pair(t, a, b)
	own, _ := member(t, a, a.id.ID)
	old := mesh.Member{ID: a.id.ID, Name: "old-name", Addrs: []string{"10.9.9.9:1"}, Generation: own.Generation + 5}
	a.n.Backend().SyncMembers(bg, b.id.ID, []mesh.Member{old})
	after, _ := member(t, a, a.id.ID)
	if after.Name != "alpha" || after.Generation != old.Generation+1 || !slices.Equal(after.Addrs, own.Addrs) {
		t.Fatalf("%+v", after)
	}
	// A tombstone of this host is never answered with a revival.
	gone := mesh.Member{ID: a.id.ID, Name: "alpha", Addrs: own.Addrs, Generation: after.Generation + 3, Removed: true}
	a.n.Backend().SyncMembers(bg, b.id.ID, []mesh.Member{gone})
	if again, _ := member(t, a, a.id.ID); again.Generation != after.Generation || again.Removed {
		t.Fatalf("%+v", again)
	}
}
