package node

import (
	"context"
	"net"
	"runtime"
	"slices"
	"sync"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/remote"
	"sugarintegration/internal/vfs"
)

// targetDialTimeout bounds the connection to this host's screen service.
const targetDialTimeout = 5 * time.Second

// remoteState is the node's share of remote control (spec
// 2026-11-22-remote-control-design.md, sections 3 and 4).
type remoteState struct {
	sem chan struct{} // one token per open tunnel served

	auditOnce sync.Once
	audit     *remote.Audit

	mu      sync.Mutex
	clients map[string]*clientEntry // dial only remote-control networks
}

func newRemoteState() remoteState {
	return remoteState{sem: make(chan struct{}, remote.MaxSessions), clients: map[string]*clientEntry{}}
}

func (n *Node) remoteAudit() *remote.Audit {
	n.rc.auditOnce.Do(func() { n.rc.audit = remote.OpenAudit(n.cfg.Dir()) })
	return n.rc.audit
}

// RemoteAllow is an allow-list entry as the Control API shows it.
type RemoteAllow struct {
	ID, Name string
	Epoch    uint64
	// Valid is false when the member was removed or re-paired since: the
	// entry lets nobody in until it is granted again.
	Valid bool
}

// RemoteStatus is this host's remote-control configuration.
type RemoteStatus struct {
	Enabled       bool
	Target        string // effective; "" if this OS has no default and none is set
	DefaultTarget bool   // Target is the OS default
	Networks      []string
	Allow         []RemoteAllow
}

// RemoteSettings changes the fields that are not nil.
type RemoteSettings struct {
	Enabled  *bool
	Target   *string // "" goes back to the OS default
	Networks *[]string
}

func (n *Node) RemoteStatus() RemoteStatus {
	c := n.cfg.Get()
	rc := c.Remote
	names := mesh.LocalNames(c.Members, n.id.ID)
	st := RemoteStatus{
		Enabled: rc.Enabled, Target: rc.EffectiveTarget(runtime.GOOS), DefaultTarget: rc.Target == "",
		Networks: slices.Clone(rc.EffectiveNetworks()), Allow: []RemoteAllow{},
	}
	for _, a := range rc.Allow {
		e := RemoteAllow{ID: a.ID, Name: names[a.ID], Epoch: a.Epoch}
		if m, err := n.activePeer(a.ID); err == nil && m.Epoch == a.Epoch {
			e.Valid = true
		}
		st.Allow = append(st.Allow, e)
	}
	return st
}

func (n *Node) SetRemote(s RemoteSettings) error {
	if s.Target != nil && *s.Target != "" {
		if err := remote.ValidateTarget(*s.Target); err != nil {
			return err
		}
	}
	if s.Networks != nil {
		if _, err := remote.ParseNetworks(*s.Networks); err != nil {
			return err
		}
	}
	_, err := n.update(func(c *config.Config) error {
		rc := &c.Remote
		if s.Target != nil {
			rc.Target = *s.Target
		}
		if s.Networks != nil {
			rc.Networks = slices.Clone(*s.Networks)
			if len(rc.Networks) == 0 {
				rc.Networks = nil
			}
		}
		if s.Enabled != nil {
			rc.Enabled = *s.Enabled
		}
		if rc.Enabled && rc.EffectiveTarget(runtime.GOOS) == "" {
			return vfs.E(vfs.CodeInvalidRequest, "this system has no screen service of its own; set a target first")
		}
		return nil
	})
	return err
}

// AllowRemote lets the peer known as name open tunnels to this host, until it
// is removed or re-paired.
func (n *Node) AllowRemote(name string) error {
	id, local, err := n.ResolvePeer(name)
	if err != nil {
		return err
	}
	if local {
		return vfs.E(vfs.CodeInvalidRequest, "this host does not need permission to control itself")
	}
	m, err := n.activePeer(id)
	if err != nil {
		return vfs.E(vfs.CodePeerNotFound, "no peer %q", name)
	}
	_, err = n.update(func(c *config.Config) error {
		c.Remote.Grant(id, m.Epoch)
		return nil
	})
	return err
}

// DenyRemote takes a peer off the allow list. It takes a name or, for an
// entry whose member is gone, an ID.
func (n *Node) DenyRemote(nameOrID string) error {
	id := nameOrID
	if rid, _, err := n.ResolvePeer(nameOrID); err == nil {
		id = rid
	}
	_, err := n.update(func(c *config.Config) error {
		if !c.Remote.Revoke(id) {
			return vfs.E(vfs.CodePeerNotFound, "%q is not allowed to control this host", nameOrID)
		}
		return nil
	})
	return err
}

// OpenTunnel decides whether member callerID, connected from remoteAddr, may
// reach this host's screen service, and connects to it. Every decision is
// logged. The session's Finish must be called.
func (n *Node) OpenTunnel(ctx context.Context, callerID, remoteAddr string) (*remote.Session, error) {
	c := n.cfg.Get()
	rc := c.Remote
	ev := remote.Event{PeerID: callerID, PeerName: mesh.LocalNames(c.Members, n.id.ID)[callerID], RemoteAddr: remoteAddr}
	who := ev.PeerName
	if who == "" {
		who = callerID
	}
	audit := n.remoteAudit()
	deny := func(code, msg string) error {
		ev.Event, ev.Reason = "denied", msg
		audit.Log(ev)
		return vfs.E(code, "%s", msg)
	}
	m, err := n.activePeer(callerID)
	if err != nil {
		return nil, deny(vfs.CodePeerUnauthorized, "not a member")
	}
	if !rc.Enabled {
		return nil, deny(vfs.CodePeerUnauthorized, "remote control is off on this host")
	}
	if !rc.Allowed(callerID, m.Epoch) {
		return nil, deny(vfs.CodePeerUnauthorized, "this host does not allow remote control from "+who)
	}
	nets, err := remote.ParseNetworks(rc.EffectiveNetworks())
	if err != nil {
		return nil, deny(vfs.CodeInternal, "the remote-control networks in the config are invalid")
	}
	if !remote.InNetworks(remoteAddr, nets) {
		return nil, deny(vfs.CodePeerUnauthorized, "remote control is only allowed over the remote-control networks (Tailscale by default); this connection came from "+remoteAddr)
	}
	target := rc.EffectiveTarget(runtime.GOOS)
	if target == "" || remote.ValidateTarget(target) != nil {
		return nil, deny(vfs.CodeInvalidRequest, "this host has no valid remote target")
	}
	select {
	case n.rc.sem <- struct{}{}:
	default:
		return nil, deny(vfs.CodeConflict, "this host already serves the most remote sessions it allows")
	}
	dctx, cancel := context.WithTimeout(ctx, targetDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", target)
	if err != nil {
		<-n.rc.sem
		return nil, deny(vfs.CodePeerOffline, "the screen service on this host is not running ("+target+")")
	}
	ev.Event = "open"
	audit.Log(ev)
	start := n.now()
	return remote.NewSession(conn, func(in, out int64) {
		<-n.rc.sem
		ev.Event, ev.Time, ev.BytesIn, ev.BytesOut = "close", time.Time{}, in, out
		ev.Seconds = n.now().Sub(start).Seconds()
		audit.Log(ev)
	}), nil
}

// RemoteClient is a client for peerID that dials only the member's addresses
// inside this host's remote-control networks, so that a tunnel never takes
// a path the target refuses (a LAN address, say) when a Tailscale one exists.
func (n *Node) RemoteClient(peerID string) (*peerapi.Client, error) {
	m, err := n.activePeer(peerID)
	if err != nil {
		return nil, err
	}
	nets, err := remote.ParseNetworks(n.cfg.Get().Remote.EffectiveNetworks())
	if err != nil {
		return nil, err
	}
	addrs := remote.DialAddrs(m.Addrs, nets)
	if len(addrs) == 0 {
		return nil, vfs.E(vfs.CodeInvalidRequest, "the peer has no address in the remote-control networks (Tailscale by default); on that host, advertise its Tailscale address")
	}
	n.rc.mu.Lock()
	defer n.rc.mu.Unlock()
	ce := n.rc.clients[peerID]
	switch {
	case ce == nil:
		ce = &clientEntry{c: peerapi.NewClient(n.id, peerID, addrs), addrs: addrs}
		n.rc.clients[peerID] = ce
	case !slices.Equal(ce.addrs, addrs):
		ce.c.SetAddrs(addrs)
		ce.addrs = addrs
	}
	return ce.c, nil
}
