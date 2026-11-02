package mesh

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestDetectAddrsFilters(t *testing.T) {
	ip := netip.MustParseAddr
	ifaces := []Iface{
		{Name: "lo", IPs: []netip.Addr{ip("127.0.0.1"), ip("::1")}},
		{Name: "docker0", IPs: []netip.Addr{ip("172.17.0.1")}},
		{Name: "br-abc", IPs: []netip.Addr{ip("172.18.0.1")}},
		{Name: "veth1", IPs: []netip.Addr{ip("10.0.0.9")}},
		{Name: "virbr0", IPs: []netip.Addr{ip("192.168.122.1")}},
		{Name: "wlan0", IPs: []netip.Addr{ip("fe80::1"), ip("169.254.1.1")}},
		{Name: "enp1s0", IPs: []netip.Addr{ip("192.168.0.5")}},
		{Name: "tailscale0", IPs: []netip.Addr{ip("100.64.0.1"), ip("fd7a::1")}},
	}
	got := DetectAddrs(ifaces, 7443)
	want := []string{"192.168.0.5:7443", "100.64.0.1:7443", "[fd7a::1]:7443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}
