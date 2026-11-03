package node

import (
	"context"
	"slices"

	"sugarintegration/internal/mesh"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

// ResolvePeer maps a local alias to a member ID. "local" and this host's own
// name are local.
func (n *Node) ResolvePeer(name string) (id string, local bool, err error) {
	if name == mesh.ReservedName {
		return n.id.ID, true, nil
	}
	c := n.cfg.Get()
	for mid, alias := range mesh.LocalNames(c.Members, n.id.ID) {
		if alias == name {
			return mid, mid == n.id.ID, nil
		}
	}
	if name == c.Name {
		return n.id.ID, true, nil
	}
	return "", false, vfs.E(vfs.CodePeerNotFound, "no peer %q", name)
}

// Client returns the client for an active member, with its current addresses.
func (n *Node) Client(peerID string) (*peerapi.Client, error) {
	m, err := n.activePeer(peerID)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	ce := n.clients[peerID]
	switch {
	case ce == nil:
		ce = &clientEntry{c: peerapi.NewClient(n.id, peerID, m.Addrs), addrs: slices.Clone(m.Addrs)}
		n.clients[peerID] = ce
	case !slices.Equal(ce.addrs, m.Addrs):
		ce.c.SetAddrs(m.Addrs) // resets the preferred address, so only on change
		ce.addrs = slices.Clone(m.Addrs)
	}
	return ce.c, nil
}

// Route is Client for operations on a peer's data: it refuses a peer that
// speaks an incompatible protocol, which Client does not so that health
// checks can still see it recover.
func (n *Node) Route(peerID string) (*peerapi.Client, error) {
	cl, err := n.Client(peerID)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	incompatible := n.status[peerID].incompatible
	n.mu.Unlock()
	if incompatible {
		return nil, vfs.E(vfs.CodePeerIncompatible, "peer runs an incompatible protocol version")
	}
	return cl, nil
}

// Shares lists a peer's shares ("local" for this host's).
func (n *Node) Shares(ctx context.Context, peer string) ([]peerapi.ShareInfo, error) {
	id, local, err := n.ResolvePeer(peer)
	if err != nil {
		return nil, err
	}
	if local {
		return n.localShares(), nil
	}
	cl, err := n.Route(id)
	if err != nil {
		return nil, err
	}
	return cl.Shares(ctx)
}

// FS returns the filesystem of a share on a peer.
func (n *Node) FS(ctx context.Context, peer, share string) (vfs.FS, error) {
	id, local, err := n.ResolvePeer(peer)
	if err != nil {
		return nil, err
	}
	if local {
		return n.SourceFS(id, share)
	}
	cl, err := n.Route(id)
	if err != nil {
		return nil, err
	}
	infos, err := cl.Shares(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(infos, func(s peerapi.ShareInfo) bool { return s.Name == share }) {
		return nil, vfs.E(vfs.CodeShareNotFound, "no share %q on %s", share, peer)
	}
	return cl.FS(share), nil
}

// SourceFS is transfer.Options.Source: the share of a member by ID, with this
// host's own ID giving the local filesystem.
func (n *Node) SourceFS(peerID, share string) (vfs.FS, error) {
	if peerID == n.id.ID {
		l, err := n.localFS(share)
		if err != nil {
			return nil, err
		}
		return l, nil
	}
	cl, err := n.Route(peerID)
	if err != nil {
		return nil, err
	}
	return cl.FS(share), nil
}

// DestFS is transfer.Options.Dest: always a local share.
func (n *Node) DestFS(share string) (transfer.Dest, error) {
	l, err := n.localFS(share)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// SourceReadOnly is transfer.Options.SourceReadOnly.
func (n *Node) SourceReadOnly(ctx context.Context, peerID, share string) (bool, error) {
	var infos []peerapi.ShareInfo
	if peerID == n.id.ID {
		infos = n.localShares()
	} else {
		cl, err := n.Route(peerID)
		if err != nil {
			return false, err
		}
		if infos, err = cl.Shares(ctx); err != nil {
			return false, err
		}
	}
	for _, s := range infos {
		if s.Name == share {
			return s.ReadOnly, nil
		}
	}
	return false, vfs.E(vfs.CodeShareNotFound, "no share %q", share)
}

// backend adapts Node to peerapi.Backend.
type backend struct{ n *Node }

var _ peerapi.Backend = backend{}

func (b backend) IsMember(id string) bool {
	if id == b.n.id.ID {
		return false
	}
	_, err := b.n.activePeer(id)
	return err == nil
}

func (b backend) Info() peerapi.Info {
	return peerapi.Info{ID: b.n.id.ID, Name: b.n.cfg.Get().Name, Protocol: peerapi.ProtocolMajor}
}

func (b backend) Shares() []peerapi.ShareInfo { return b.n.localShares() }

func (b backend) ShareFS(name string) (vfs.FS, error) {
	l, err := b.n.localFS(name)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (b backend) Pair(ctx context.Context, callerID string, r peerapi.PairRequest) (peerapi.PairResponse, error) {
	return b.n.pair(ctx, callerID, r)
}

func (b backend) SyncMembers(ctx context.Context, callerID string, remote []mesh.Member) ([]mesh.Member, error) {
	return b.n.syncMembers(callerID, remote)
}

var errNoJobs = vfs.E(vfs.CodeInternal, "transfers are not available")

// SubmitJob accepts a job that runs here, its destination, on behalf of the
// calling member.
func (b backend) SubmitJob(callerID string, s transfer.Spec) (transfer.Job, error) {
	m := b.n.jobs.Load()
	if m == nil {
		return transfer.Job{}, errNoJobs
	}
	if s.Dst.Peer != b.n.id.ID {
		return transfer.Job{}, vfs.E(vfs.CodeInvalidRequest, "jobs run on their destination host")
	}
	if s.Src.Peer != b.n.id.ID {
		if _, err := b.n.activePeer(s.Src.Peer); err != nil {
			return transfer.Job{}, vfs.E(vfs.CodeInvalidRequest, "source host is not a member")
		}
	}
	return m.Submit(callerID, s)
}

func (b backend) ListJobs(origin string) []transfer.Job {
	if m := b.n.jobs.Load(); m != nil {
		return m.List(origin)
	}
	return nil
}

func (b backend) CancelJob(id string) error {
	if m := b.n.jobs.Load(); m != nil {
		return m.Cancel(id)
	}
	return errNoJobs
}

func (b backend) RetryJob(id string) error {
	if m := b.n.jobs.Load(); m != nil {
		return m.Retry(id)
	}
	return errNoJobs
}
