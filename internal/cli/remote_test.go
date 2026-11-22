package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRemoteSettingsFlow(t *testing.T) {
	a, b, _, _ := pair(t)
	out := b.ok("remote", "status")
	if !strings.Contains(out, "remote control: disabled") || !strings.Contains(out, "100.64.0.0/10") || !strings.Contains(out, "allowed peers: none") {
		t.Fatalf("default status:\n%s", out)
	}
	b.ok("remote", "target", "127.0.0.1:5999")
	b.ok("remote", "enable")
	b.ok("remote", "allow", "a")
	b.ok("remote", "networks", "127.0.0.0/8")
	var st struct {
		Enabled  bool
		Target   string
		Networks []string
		Allow    []struct {
			Name  string
			Valid bool
		}
	}
	if err := json.Unmarshal([]byte(b.ok("remote", "status", "--json")), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.Target != "127.0.0.1:5999" || len(st.Networks) != 1 || len(st.Allow) != 1 || st.Allow[0].Name != "a" || !st.Allow[0].Valid {
		t.Fatalf("status %+v", st)
	}
	if out := b.ok("remote", "status"); !strings.Contains(out, "ALLOWED PEER") || !strings.Contains(out, "active") {
		t.Fatalf("status table:\n%s", out)
	}

	if code, _, errs := b.run("remote", "target", "10.0.0.1:5900"); code != 1 || !strings.Contains(errs, "loopback") {
		t.Fatalf("non-loopback target: exit %d %q", code, errs)
	}
	if code, _, _ := a.run("remote", "connect", "local"); code != 1 {
		t.Fatalf("connect to self: exit %d", code)
	}
	b.ok("remote", "deny", "a")
	b.ok("remote", "networks")
	b.ok("remote", "disable")
	if out := b.ok("remote", "status"); !strings.Contains(out, "disabled") || !strings.Contains(out, "none") || !strings.Contains(out, "100.64.0.0/10") {
		t.Fatalf("after reset:\n%s", out)
	}
}
