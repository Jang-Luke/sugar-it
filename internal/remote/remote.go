// Package remote holds the remote-control policy (spec
// 2026-11-22-remote-control-design.md, sections 3 and 4): who may open a
// tunnel to this host's screen service, from which networks, to which
// address, and the access log.
package remote

import (
	"net"
	"net/netip"
	"slices"
	"sync"

	"sugarintegration/internal/vfs"
)

// MaxSessions is how many tunnels a host serves at once.
const MaxSessions = 4

// DefaultNetworks are Tailscale's address ranges: IPv4 CGNAT space and the
// tailnet IPv6 ULA prefix.
var DefaultNetworks = []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}

// Allow lets one member in, for as long as its epoch stays the same. A
// removal and a re-pairing both raise the epoch, so neither keeps the grant.
type Allow struct {
	ID    string `json:"id"`
	Epoch uint64 `json:"epoch"`
}

// Config is the "remote" section of config.json. The zero value is off.
type Config struct {
	Enabled  bool     `json:"enabled"`
	Allow    []Allow  `json:"allow,omitempty"`
	Target   string   `json:"target,omitempty"`   // "" = DefaultTarget(GOOS)
	Networks []string `json:"networks,omitempty"` // nil = DefaultNetworks
}

func (c Config) Clone() Config {
	c.Allow = slices.Clone(c.Allow)
	c.Networks = slices.Clone(c.Networks)
	return c
}

// DefaultTarget is the screen service of an OS: macOS Screen Sharing, or the
// sugar-it remote agent on Windows. Other systems have none.
func DefaultTarget(goos string) string {
	switch goos {
	case "darwin":
		return "127.0.0.1:5900"
	case "windows":
		return "127.0.0.1:7446"
	}
	return ""
}

// EffectiveTarget is the address tunnels connect to, "" if there is none.
func (c Config) EffectiveTarget(goos string) string {
	if c.Target != "" {
		return c.Target
	}
	return DefaultTarget(goos)
}

// EffectiveNetworks are the networks tunnels may come from or go over.
func (c Config) EffectiveNetworks() []string {
	if len(c.Networks) == 0 {
		return DefaultNetworks
	}
	return c.Networks
}

// ValidateTarget accepts only a loopback host:port: a tunnel must never become
// a way into the target's network.
func ValidateTarget(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err == nil && port != "" && port != "0" {
		if ip, perr := netip.ParseAddr(host); host == "localhost" || (perr == nil && ip.Unmap().IsLoopback()) {
			return nil
		}
	}
	return vfs.E(vfs.CodeInvalidRequest, "the remote target must be a loopback address such as 127.0.0.1:5900, not %q", addr)
}

// ParseNetworks parses CIDR prefixes.
func ParseNetworks(nets []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(nets))
	for _, s := range nets {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, vfs.E(vfs.CodeInvalidRequest, "%q is not a network such as 100.64.0.0/10", s)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// InNetworks reports whether the host of hostport is an IP inside one of
// nets. Unparsable input is outside.
func InNetworks(hostport string, nets []netip.Prefix) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	for _, p := range nets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// DialAddrs keeps the member addresses a controller may dial: IPs inside
// nets, and host names, which the target's own check judges by the address
// they resolve to.
func DialAddrs(addrs []string, nets []netip.Prefix) []string {
	var out []string
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		if _, perr := netip.ParseAddr(host); perr != nil || InNetworks(a, nets) {
			out = append(out, a)
		}
	}
	return out
}

// Allowed reports whether member id at epoch is on the allow list.
func (c Config) Allowed(id string, epoch uint64) bool {
	return slices.Contains(c.Allow, Allow{ID: id, Epoch: epoch})
}

// Grant puts id at epoch on the list, replacing an entry for an older epoch.
func (c *Config) Grant(id string, epoch uint64) {
	c.Revoke(id)
	c.Allow = append(c.Allow, Allow{ID: id, Epoch: epoch})
}

// Revoke takes id off the list. It reports whether it was there.
func (c *Config) Revoke(id string) bool {
	n := len(c.Allow)
	c.Allow = slices.DeleteFunc(c.Allow, func(a Allow) bool { return a.ID == id })
	return len(c.Allow) != n
}

// Session is a tunnel's connection to the screen service.
type Session struct {
	Conn   net.Conn
	finish func(in, out int64)
	once   sync.Once
}

func NewSession(conn net.Conn, finish func(in, out int64)) *Session {
	return &Session{Conn: conn, finish: finish}
}

// Finish closes the connection and records the session's end, once.
func (s *Session) Finish(in, out int64) {
	s.once.Do(func() {
		s.Conn.Close()
		if s.finish != nil {
			s.finish(in, out)
		}
	})
}
