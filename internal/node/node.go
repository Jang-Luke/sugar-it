// Package node keeps the mesh alive: it gossips the member list, checks peer
// health, handles pairing, and routes peer names to filesystems. It
// implements peerapi.Backend (see Backend).
package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/identity"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

const (
	defaultGossipEvery = 60 * time.Second
	defaultHealthEvery = 15 * time.Second
	defaultCallTimeout = 10 * time.Second
	defaultInviteTTL   = 10 * time.Minute
)

// PeerStatus is what this host knows about one other member.
type PeerStatus struct {
	ID, Name     string // Name is the local alias
	Addrs        []string
	Online       bool
	Incompatible bool // a different major protocol version
	LastSeen     time.Time
}

type peerState struct {
	online, incompatible bool
	lastSeen             time.Time
}

type openShare struct {
	fs   *shares.LocalFS
	spec shares.Spec
	info os.FileInfo // the directory the root was opened on
}

// Node is one host's view of the mesh.
type Node struct {
	cfg    *config.Store
	id     *identity.Identity
	tokens *mesh.Tokens

	// Tests set these before Run.
	now                                   func() time.Time
	ifaces                                func() ([]mesh.Iface, error)
	gossipEvery, healthEvery, callTimeout time.Duration

	gossipKick, healthKick chan struct{}

	mu      sync.Mutex                  // never held while calling into cfg
	seen    struct{ epoch, gen uint64 } // highest active record any peer holds of this host (R7)
	status  map[string]peerState
	clients map[string]*clientEntry
	running bool

	fsMu    sync.Mutex
	fsCache map[string]*openShare

	jobs atomic.Pointer[transfer.Manager]
}

type clientEntry struct {
	c     *peerapi.Client
	addrs []string
}

var errNoChange = errors.New("no change")

func internal(err error) error {
	var ve *vfs.Error
	if errors.As(err, &ve) {
		return err
	}
	return vfs.E(vfs.CodeInternal, "%v", err)
}

// New returns a node over cfg. It records this host in the member list.
func New(cfg *config.Store, id *identity.Identity) (*Node, error) {
	if !mesh.ValidName(cfg.Get().Name) {
		return nil, vfs.E(vfs.CodeInvalidRequest, "config name %q is not a valid host name", cfg.Get().Name)
	}
	n := &Node{
		cfg: cfg, id: id,
		now:         time.Now,
		ifaces:      mesh.SystemIfaces,
		gossipEvery: defaultGossipEvery, healthEvery: defaultHealthEvery, callTimeout: defaultCallTimeout,
		gossipKick: make(chan struct{}, 1), healthKick: make(chan struct{}, 1),
		status:  map[string]peerState{},
		clients: map[string]*clientEntry{},
		fsCache: map[string]*openShare{},
	}
	n.tokens = mesh.NewTokens(func() time.Time { return n.now() })
	if _, err := n.refreshSelf(); err != nil {
		return nil, err
	}
	return n, nil
}

// AttachJobs gives the node the transfer manager that serves job requests.
func (n *Node) AttachJobs(m *transfer.Manager) { n.jobs.Store(m) }

// ID is this host's ID.
func (n *Node) ID() string { return n.id.ID }

// Backend returns the peerapi.Backend for this node. It is a separate value
// because the Backend's Shares() clashes with Node.Shares(ctx, peer).
func (n *Node) Backend() peerapi.Backend { return backend{n} }

func (n *Node) kick() {
	for _, c := range []chan struct{}{n.gossipKick, n.healthKick} {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// update runs fn on the config; errNoChange from fn skips the write.
func (n *Node) update(fn func(*config.Config) error) (changed bool, err error) {
	err = n.cfg.Update(func(c *config.Config) error {
		if err := fn(c); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, errNoChange) {
		return false, nil
	}
	if err != nil {
		return false, internal(err)
	}
	return changed, nil
}

// nextGen is the generation for a change this host makes to its own record:
// above the record's own value and above any value a peer holds (R7).
// Only generations in this host's own epoch count; a record of an older
// epoch is irrelevant and tombstones are never observed.
func (n *Node) nextGen(ownEpoch, own uint64) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen.epoch == ownEpoch {
		own = max(own, n.seen.gen)
	}
	return own + 1
}

// observeGen notes an active record of this host held by a peer.
func (n *Node) observeGen(epoch, gen uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case epoch > n.seen.epoch:
		n.seen.epoch, n.seen.gen = epoch, gen
	case epoch == n.seen.epoch:
		n.seen.gen = max(n.seen.gen, gen)
	}
}

func indexOf(ms []mesh.Member, id string) int {
	return slices.IndexFunc(ms, func(m mesh.Member) bool { return m.ID == id })
}

func normAddrs(a []string) []string {
	out := slices.Clone(a)
	slices.Sort(out)
	return slices.Compact(out)
}

// selfAddrs is what this host advertises.
func (n *Node) selfAddrs(c config.Config) []string {
	if len(c.AdvertiseAddrs) > 0 {
		return normAddrs(c.AdvertiseAddrs)
	}
	ifs, err := n.ifaces()
	if err != nil {
		return nil
	}
	return normAddrs(mesh.DetectAddrs(ifs, listenPort(c.ListenAddr)))
}

// refreshSelf makes this host's member record match its name and addresses,
// raising its generation if they changed.
func (n *Node) refreshSelf() (bool, error) {
	changed, err := n.update(func(c *config.Config) error {
		addrs := n.selfAddrs(*c)
		i := indexOf(c.Members, n.id.ID)
		if i < 0 {
			c.Members = append(c.Members, mesh.Member{ID: n.id.ID, Name: c.Name, Addrs: addrs, Generation: n.nextGen(0, 0)})
			return nil
		}
		m := &c.Members[i]
		if m.Name == c.Name && !m.Removed && slices.Equal(normAddrs(m.Addrs), addrs) {
			return errNoChange
		}
		m.Name, m.Addrs, m.Removed = c.Name, addrs, false
		m.Generation = n.nextGen(m.Epoch, m.Generation)
		return nil
	})
	if changed {
		n.kick()
	}
	return changed, err
}

func (n *Node) selfMember() mesh.Member {
	c := n.cfg.Get()
	if i := indexOf(c.Members, n.id.ID); i >= 0 {
		return c.Members[i]
	}
	return mesh.Member{ID: n.id.ID, Name: c.Name}
}

// Peers lists the other active members, by local alias.
func (n *Node) Peers() []PeerStatus {
	c := n.cfg.Get()
	names := mesh.LocalNames(c.Members, n.id.ID)
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []PeerStatus
	for _, m := range c.Members {
		if m.ID == n.id.ID || m.Removed {
			continue
		}
		st := n.status[m.ID]
		out = append(out, PeerStatus{ID: m.ID, Name: names[m.ID], Addrs: m.Addrs,
			Online: st.online && !st.incompatible, Incompatible: st.incompatible, LastSeen: st.lastSeen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (n *Node) peerStatus(id string) (PeerStatus, bool) {
	for _, p := range n.Peers() {
		if p.ID == id {
			return p, true
		}
	}
	return PeerStatus{}, false
}

// Invite returns a one-time invitation string valid for ttl (default 10m).
func (n *Node) Invite(ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = defaultInviteTTL
	}
	self := n.selfMember()
	if len(self.Addrs) == 0 {
		return "", vfs.E(vfs.CodeInvalidRequest, "this host has no address to invite to; set advertise_addrs")
	}
	inv := mesh.Invite{ID: n.id.ID, Name: self.Name, Addrs: self.Addrs, Token: n.tokens.Issue(ttl), Expires: n.now().Add(ttl)}
	return inv.Encode(), nil
}

// RemovePeer removes the member known locally as name (a tombstone).
func (n *Node) RemovePeer(name string) error {
	id, local, err := n.ResolvePeer(name)
	if err != nil {
		return err
	}
	if local {
		return vfs.E(vfs.CodeInvalidRequest, "a host cannot remove itself")
	}
	_, err = n.update(func(c *config.Config) error {
		i := indexOf(c.Members, id)
		if i < 0 || c.Members[i].Removed {
			return vfs.E(vfs.CodePeerNotFound, "no peer %q", name)
		}
		c.Members[i].Removed = true
		c.Members[i].Generation++
		c.Members[i].Epoch++ // only removal and re-pairing raise the epoch
		return nil
	})
	if err != nil {
		return err
	}
	n.forgetInactive()
	n.kick()
	return nil
}

// RenameSelf changes this host's name. Only the owner may, and the higher
// generation carries it to the others.
func (n *Node) RenameSelf(newName string) error {
	if !mesh.ValidName(newName) {
		return vfs.E(vfs.CodeInvalidRequest, "%q is not a valid host name (a-z, 0-9, -; 1-32 chars; not \"local\")", newName)
	}
	if err := n.cfg.Update(func(c *config.Config) error { c.Name = newName; return nil }); err != nil {
		return internal(err)
	}
	_, err := n.refreshSelf()
	return err
}

// ---- shares ----

// AddShare registers a local share.
func (n *Node) AddShare(s shares.Spec) error {
	if !mesh.ValidName(s.Name) {
		return vfs.E(vfs.CodeInvalidRequest, "%q is not a valid share name (a-z, 0-9, -; 1-32 chars; not \"local\")", s.Name)
	}
	if s.Path == "" {
		return vfs.E(vfs.CodeInvalidRequest, "share path is empty")
	}
	abs, err := filepath.Abs(s.Path)
	if err != nil {
		return vfs.E(vfs.CodeInvalidRequest, "bad share path: %v", err)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return vfs.E(vfs.CodeInvalidRequest, "share path %q is not an existing directory", abs)
	}
	s.Path = abs
	_, err = n.update(func(c *config.Config) error {
		if slices.ContainsFunc(c.Shares, func(x shares.Spec) bool { return x.Name == s.Name }) {
			return vfs.E(vfs.CodeExists, "share %q already exists", s.Name)
		}
		if err := shares.CheckOverlap(c.Shares, s.Path, runtime.GOOS); err != nil {
			return err
		}
		c.Shares = append(c.Shares, s)
		return nil
	})
	return err
}

// RemoveShare unregisters a local share and closes its open filesystem.
func (n *Node) RemoveShare(name string) error {
	_, err := n.update(func(c *config.Config) error {
		i := slices.IndexFunc(c.Shares, func(x shares.Spec) bool { return x.Name == name })
		if i < 0 {
			return vfs.E(vfs.CodeShareNotFound, "no share %q", name)
		}
		c.Shares = slices.Delete(c.Shares, i, i+1)
		return nil
	})
	if err != nil {
		return err
	}
	n.fsMu.Lock()
	if e := n.fsCache[name]; e != nil {
		e.fs.Close()
		delete(n.fsCache, name)
	}
	n.fsMu.Unlock()
	return nil
}

func (n *Node) shareSpec(name string) (shares.Spec, bool) {
	for _, s := range n.cfg.Get().Shares {
		if s.Name == name {
			return s, true
		}
	}
	return shares.Spec{}, false
}

func (n *Node) localShares() []peerapi.ShareInfo {
	out := []peerapi.ShareInfo{}
	for _, s := range n.cfg.Get().Shares {
		out = append(out, peerapi.ShareInfo{Name: s.Name, ReadOnly: s.ReadOnly})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// localFS opens a share on first use and keeps it. A failed open is not
// cached, and a cached root whose directory has gone away or been replaced is
// dropped, so an unplugged disk only fails its own share until it returns.
func (n *Node) localFS(name string) (*shares.LocalFS, error) {
	spec, ok := n.shareSpec(name)
	if !ok {
		return nil, vfs.E(vfs.CodeShareNotFound, "no share %q", name)
	}
	n.fsMu.Lock()
	defer n.fsMu.Unlock()
	if e := n.fsCache[name]; e != nil {
		if fi, err := os.Stat(spec.Path); err == nil && e.spec == spec && os.SameFile(fi, e.info) {
			return e.fs, nil
		}
		e.fs.Close()
		delete(n.fsCache, name)
	}
	l, err := shares.Open(spec)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(spec.Path)
	if err != nil {
		l.Close()
		return nil, vfs.E(vfs.CodeNotFound, "share directory is not available")
	}
	n.fsCache[name] = &openShare{fs: l, spec: spec, info: fi}
	return l, nil
}

// Run keeps the mesh alive until ctx is done: it gossips the member list
// every 60s and on any change, and checks peer health every 15s. It returns
// after all of its goroutines have stopped.
func (n *Node) Run(ctx context.Context) error {
	n.mu.Lock()
	if n.running {
		n.mu.Unlock()
		return vfs.E(vfs.CodeInvalidRequest, "node is already running")
	}
	n.running = true
	n.mu.Unlock()
	defer func() { n.mu.Lock(); n.running = false; n.mu.Unlock() }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n.loop(ctx, n.gossipEvery, n.gossipKick, n.gossipRound)
	}()
	go func() {
		defer wg.Done()
		n.loop(ctx, n.healthEvery, n.healthKick, func(ctx context.Context) {
			n.refreshSelf()
			n.healthRound(ctx)
		})
	}()
	wg.Wait()
	return nil
}

func (n *Node) loop(ctx context.Context, every time.Duration, kick <-chan struct{}, f func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		f(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}
