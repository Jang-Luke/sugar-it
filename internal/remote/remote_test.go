package remote

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestValidateTarget(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:5900", "[::1]:7446", "localhost:5900", "127.8.9.1:1"} {
		if err := ValidateTarget(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "5900", ":5900", "0.0.0.0:5900", "192.168.1.2:5900", "100.64.0.1:22", "example.com:5900", "127.0.0.1:0", "127.0.0.1:"} {
		if err := ValidateTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNetworks(t *testing.T) {
	nets, err := ParseNetworks(DefaultNetworks)
	if err != nil {
		t.Fatal(err)
	}
	in := []string{"100.64.0.1:7443", "100.127.255.254:1", "[fd7a:115c:a1e0::1]:7443", "[::ffff:100.100.1.1]:7443", "100.101.102.103"}
	out := []string{"100.128.0.1:7443", "192.168.0.5:7443", "127.0.0.1:7443", "[fd7a:115c:a1e1::1]:7443", "nas.local:7443", "garbage"}
	for _, a := range in {
		if !InNetworks(a, nets) {
			t.Errorf("%q should be inside", a)
		}
	}
	for _, a := range out {
		if InNetworks(a, nets) {
			t.Errorf("%q should be outside", a)
		}
	}
	if _, err := ParseNetworks([]string{"10.0.0.0"}); err == nil {
		t.Error("a bare IP is not a network")
	}
	got := DialAddrs([]string{"192.168.0.5:7443", "100.64.0.9:7443", "nas.tail1234.ts.net:7443", "bad"}, nets)
	if want := []string{"100.64.0.9:7443", "nas.tail1234.ts.net:7443"}; !slices.Equal(got, want) {
		t.Errorf("DialAddrs = %v, want %v", got, want)
	}
}

func TestEffective(t *testing.T) {
	var c Config
	if c.EffectiveTarget("darwin") != "127.0.0.1:5900" || c.EffectiveTarget("windows") != "127.0.0.1:7446" || c.EffectiveTarget("linux") != "" {
		t.Error("default targets")
	}
	c.Target = "127.0.0.1:1"
	if c.EffectiveTarget("darwin") != "127.0.0.1:1" {
		t.Error("explicit target")
	}
	if !slices.Equal(c.EffectiveNetworks(), DefaultNetworks) {
		t.Error("default networks")
	}
}

func TestAllowList(t *testing.T) {
	var c Config
	c.Grant("a", 2)
	c.Grant("b", 4)
	if !c.Allowed("a", 2) || c.Allowed("a", 4) || c.Allowed("c", 0) {
		t.Fatal("allowed")
	}
	c.Grant("a", 4) // a re-pairing granted again
	if c.Allowed("a", 2) || !c.Allowed("a", 4) || len(c.Allow) != 2 {
		t.Fatalf("regrant: %+v", c.Allow)
	}
	if !c.Revoke("a") || c.Revoke("a") || c.Allowed("a", 4) {
		t.Fatal("revoke")
	}
	d := c.Clone()
	d.Allow[0].Epoch = 99
	if c.Allow[0].Epoch == 99 {
		t.Fatal("clone shares the slice")
	}
}

func TestAuditAppendsAndRotates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, LogName)
	big := make([]byte, logRotateSize+1)
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatal(err)
	}
	a := OpenAudit(dir)
	a.Log(Event{Event: "denied", PeerID: "x", Reason: "off"})
	a.Log(Event{Event: "open", PeerID: "x"})
	if fi, err := os.Stat(p + ".1"); err != nil || fi.Size() != int64(len(big)) {
		t.Fatalf("rotated file: %v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var evs []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	if len(evs) != 2 || evs[0].Event != "denied" || evs[0].Reason != "off" || evs[1].Event != "open" || evs[0].Time.IsZero() {
		t.Fatalf("events %+v", evs)
	}
	if fi, _ := os.Stat(p); !isWindows() && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }
