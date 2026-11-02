package mesh

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
)

type Iface struct {
	Name string
	IPs  []netip.Addr
}

var skippedIfacePrefixes = []string{"docker", "br-", "veth", "virbr"}

// DetectAddrs returns host:port strings for the addresses worth advertising,
// skipping loopback, link-local, unspecified and container/VM bridge interfaces.
func DetectAddrs(ifaces []Iface, port int) []string {
	var out []string
	p := strconv.Itoa(port)
next:
	for _, ifc := range ifaces {
		for _, pre := range skippedIfacePrefixes {
			if strings.HasPrefix(ifc.Name, pre) {
				continue next
			}
		}
		if ifc.Name == "lo" {
			continue
		}
		for _, ip := range ifc.IPs {
			ip = ip.Unmap()
			if !ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
				ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			out = append(out, net.JoinHostPort(ip.WithZone("").String(), p))
		}
	}
	return out
}

// SystemIfaces lists the up network interfaces of this machine.
func SystemIfaces() ([]Iface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Iface
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		e := Iface{Name: ifc.Name}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
					e.IPs = append(e.IPs, ip)
				}
			}
		}
		out = append(out, e)
	}
	return out, nil
}
