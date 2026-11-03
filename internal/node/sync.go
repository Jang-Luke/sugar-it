package node

import (
	"context"
	"net"
	"slices"
	"strconv"
	"sync"

	"sugarintegration/internal/config"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/vfs"
)

func listenPort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// sanitize drops members a remote must not be able to add: no ID, or a name
// that is not a valid host name. Gossip and invites are untrusted input.
func sanitize(in []mesh.Member) []mesh.Member {
	out := make([]mesh.Member, 0, len(in))
	for _, m := range in {
		if m.ID == "" || !mesh.ValidName(m.Name) {
			continue
		}
		m.Addrs = slices.Clone(m.Addrs)
		out = append(out, m)
	}
	return out
}

func sameContent(a, b mesh.Member) bool {
	return a.Name == b.Name && slices.Equal(normAddrs(a.Addrs), normAddrs(b.Addrs))
}

// mergeIn folds a peer's member list into ours. adopt, if set, is the record
// of the inviter from a fresh pairing, which must end up active.
func (n *Node) mergeIn(remote []mesh.Member, adopt *mesh.Member) (bool, error) {
	remote = sanitize(remote)
	var theirs *mesh.Member // the best active record a peer holds of this host
	for i, r := range remote {
		if r.ID != n.id.ID {
			continue
		}
		n.observeGen(r.Generation)
		if !r.Removed && (theirs == nil || r.Generation > theirs.Generation) {
			theirs = &remote[i]
		}
	}
	changed, err := n.update(func(c *config.Config) error {
		merged, ch := mesh.Merge(c.Members, remote, n.id.ID)
		if adopt != nil {
			if i := indexOf(merged, adopt.ID); i < 0 {
				merged, ch = append(merged, *adopt), true
			} else if merged[i].Removed {
				merged[i] = mesh.Member{ID: adopt.ID, Name: adopt.Name, Addrs: normAddrs(append(slices.Clone(merged[i].Addrs), adopt.Addrs...)),
					Generation: merged[i].Generation + 1}
				ch = true
			}
		}
		// A peer holds a different record of this host at or above ours: it
		// would win, so supersede it with the truth.
		if i := indexOf(merged, n.id.ID); i >= 0 && theirs != nil {
			own := &merged[i]
			if theirs.Generation >= own.Generation && theirs.Generation < ^uint64(0) && !sameContent(*theirs, *own) {
				own.Generation = theirs.Generation + 1
				ch = true
			}
		}
		if !ch {
			return errNoChange
		}
		c.Members = merged
		return nil
	})
	if changed {
		n.forgetInactive()
		n.kick()
	}
	return changed, err
}

// forgetInactive drops clients and status of members that are gone.
func (n *Node) forgetInactive() {
	active := map[string]bool{}
	for _, m := range n.cfg.Get().Members {
		if !m.Removed {
			active[m.ID] = true
		}
	}
	n.mu.Lock()
	var stale []*peerapi.Client
	for id, ce := range n.clients {
		if !active[id] {
			stale = append(stale, ce.c)
			delete(n.clients, id)
		}
	}
	for id := range n.status {
		if !active[id] {
			delete(n.status, id)
		}
	}
	n.mu.Unlock()
	for _, c := range stale {
		c.SetAddrs(nil) // closes idle connections
	}
}

// activePeers lists the other active members.
func (n *Node) activePeers() []mesh.Member {
	var out []mesh.Member
	for _, m := range n.cfg.Get().Members {
		if m.ID != n.id.ID && !m.Removed {
			out = append(out, m)
		}
	}
	return out
}

// forPeers runs f for every active peer concurrently and waits for all.
func (n *Node) forPeers(ctx context.Context, f func(ctx context.Context, m mesh.Member, cl *peerapi.Client)) {
	var wg sync.WaitGroup
	for _, m := range n.activePeers() {
		cl, err := n.Client(m.ID)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, n.callTimeout)
			defer cancel()
			f(cctx, m, cl)
		}()
	}
	wg.Wait()
}

// gossipRound exchanges member lists with every reachable, compatible peer.
func (n *Node) gossipRound(ctx context.Context) {
	mine := n.cfg.Get().Members
	n.forPeers(ctx, func(ctx context.Context, m mesh.Member, cl *peerapi.Client) {
		n.mu.Lock()
		skip := n.status[m.ID].incompatible
		n.mu.Unlock()
		if skip {
			return
		}
		theirs, err := cl.SyncMembers(ctx, mine)
		if err != nil || ctx.Err() != nil {
			return
		}
		n.mergeIn(theirs, nil)
	})
}

// healthRound probes every peer with GET /v1/info.
func (n *Node) healthRound(ctx context.Context) {
	n.forPeers(ctx, func(ctx context.Context, m mesh.Member, cl *peerapi.Client) {
		info, err := cl.Info(ctx)
		if ctx.Err() != nil && err != nil {
			return // shutting down or timed out: no news
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		switch {
		case err != nil || info.ID != m.ID:
			n.status[m.ID] = peerState{lastSeen: n.status[m.ID].lastSeen}
		case info.Protocol != peerapi.ProtocolMajor:
			n.status[m.ID] = peerState{incompatible: true, lastSeen: n.now()}
		default:
			n.status[m.ID] = peerState{online: true, lastSeen: n.now()}
		}
	})
}

// ensureActive reports peer_not_found unless id is an active peer.
func (n *Node) activePeer(id string) (mesh.Member, error) {
	for _, m := range n.activePeers() {
		if m.ID == id {
			return m, nil
		}
	}
	return mesh.Member{}, vfs.E(vfs.CodePeerNotFound, "no such peer")
}
