package node

import (
	"context"
	"slices"

	"sugarintegration/internal/config"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/vfs"
)

// Join pairs with the host that made invite and merges its member list.
func (n *Node) Join(ctx context.Context, invite string) (PeerStatus, error) {
	inv, err := mesh.DecodeInvite(invite)
	if err != nil {
		return PeerStatus{}, vfs.E(vfs.CodeInvalidRequest, "%v", err)
	}
	switch {
	case !inv.Expires.After(n.now()):
		return PeerStatus{}, vfs.E(vfs.CodePeerUnauthorized, "invite expired")
	case !mesh.ValidName(inv.Name):
		return PeerStatus{}, vfs.E(vfs.CodeInvalidRequest, "invite has an invalid host name")
	case !validID(inv.ID):
		return PeerStatus{}, vfs.E(vfs.CodeInvalidRequest, "invite has an invalid host ID")
	case inv.ID == n.id.ID:
		return PeerStatus{}, vfs.E(vfs.CodeInvalidRequest, "cannot join your own invite")
	}
	self := n.selfMember()
	// The client is pinned to the invite's ID: a host that does not hold that
	// key never sees the token.
	cl := peerapi.NewClient(n.id, inv.ID, inv.Addrs)
	resp, err := cl.Pair(ctx, peerapi.PairRequest{Token: inv.Token, Name: self.Name, Addrs: self.Addrs})
	if err != nil {
		return PeerStatus{}, err
	}
	their := resp.Self
	their.ID = inv.ID // the pinned certificate decides who this is
	if !mesh.ValidName(their.Name) {
		their.Name = inv.Name
	}
	their.Addrs = normAddrs(append(slices.Clone(their.Addrs), inv.Addrs...))
	if _, err := n.mergeIn(append(slices.Clone(resp.Members), their), &their); err != nil {
		return PeerStatus{}, err
	}
	n.mu.Lock()
	n.status[inv.ID] = peerState{online: true, lastSeen: n.now()}
	n.mu.Unlock()
	n.kick()
	st, ok := n.peerStatus(inv.ID)
	if !ok {
		return PeerStatus{}, vfs.E(vfs.CodeInternal, "paired host is missing from the member list")
	}
	return st, nil
}

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// pair handles POST /v1/pair on the inviting side.
func (n *Node) pair(ctx context.Context, callerID string, r peerapi.PairRequest) (peerapi.PairResponse, error) {
	if callerID == n.id.ID {
		return peerapi.PairResponse{}, vfs.E(vfs.CodeInvalidRequest, "cannot pair with itself")
	}
	if !mesh.ValidName(r.Name) {
		return peerapi.PairResponse{}, vfs.E(vfs.CodeInvalidRequest, "invalid host name")
	}
	if !n.tokens.Consume(r.Token) {
		return peerapi.PairResponse{}, vfs.E(vfs.CodePeerUnauthorized, "invalid or expired invite")
	}
	_, err := n.update(func(c *config.Config) error {
		m := mesh.Member{ID: callerID, Name: r.Name, Addrs: normAddrs(r.Addrs), Generation: 1}
		if i := indexOf(c.Members, callerID); i >= 0 {
			m.Generation = c.Members[i].Generation + 1 // re-registration after removal
			c.Members[i] = m
		} else {
			c.Members = append(c.Members, m)
		}
		return nil
	})
	if err != nil {
		return peerapi.PairResponse{}, err
	}
	n.kick()
	return peerapi.PairResponse{Self: n.selfMember(), Members: n.cfg.Get().Members}, nil
}

// syncMembers handles POST /v1/members/sync.
func (n *Node) syncMembers(callerID string, remote []mesh.Member) ([]mesh.Member, error) {
	if _, err := n.mergeIn(remote, nil); err != nil {
		return nil, err
	}
	return n.cfg.Get().Members, nil
}
