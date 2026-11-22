package control_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sugarintegration/internal/control"
	"sugarintegration/internal/node"
	"sugarintegration/internal/remote"
	"sugarintegration/internal/vfs"
)

var loopbackNets = []string{"127.0.0.0/8"}

func ptr[T any](v T) *T { return &v }

// echo is a stand-in for a screen service.
func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// remotePair makes a the controller and b the target, both on loopback
// networks, with b's screen service at target and a allowed.
func remotePair(t *testing.T, target string) (a, b *host) {
	t.Helper()
	a, b = newHost(t, "a"), newHost(t, "b")
	pair(t, a, b)
	eventually(t, "b sees a", func() bool { return b.c.AllowRemote(bg, "a") == nil })
	if err := b.c.SetRemote(bg, node.RemoteSettings{Enabled: ptr(true), Target: ptr(target), Networks: ptr(loopbackNets)}); err != nil {
		t.Fatal(err)
	}
	if err := a.c.SetRemote(bg, node.RemoteSettings{Networks: ptr(loopbackNets)}); err != nil {
		t.Fatal(err)
	}
	return a, b
}

type forwarding struct {
	addr   string
	events chan control.RemoteEvent
	cancel context.CancelFunc
	done   chan error
}

func connect(t *testing.T, h *host, peer string) *forwarding {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	f := &forwarding{events: make(chan control.RemoteEvent, 32), cancel: cancel, done: make(chan error, 1)}
	t.Cleanup(cancel)
	go func() {
		f.done <- h.c.RemoteConnect(ctx, peer, "", func(ev control.RemoteEvent) { f.events <- ev })
	}()
	select {
	case ev := <-f.events:
		if ev.Event != "listening" {
			t.Fatalf("first event %+v", ev)
		}
		f.addr = ev.Addr
	case err := <-f.done:
		t.Fatalf("connect: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no listening event")
	}
	return f
}

func (f *forwarding) next(t *testing.T) control.RemoteEvent {
	t.Helper()
	select {
	case ev := <-f.events:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
	return control.RemoteEvent{}
}

func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != msg {
		t.Fatalf("got %q", got)
	}
}

func auditEvents(t *testing.T, h *host) []remote.Event {
	t.Helper()
	f, err := os.Open(filepath.Join(h.dir, remote.LogName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []remote.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e remote.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func lastAudit(t *testing.T, h *host) remote.Event {
	evs := auditEvents(t, h)
	if len(evs) == 0 {
		return remote.Event{}
	}
	return evs[len(evs)-1]
}

func TestRemoteTunnelEndToEnd(t *testing.T) {
	a, b := remotePair(t, echo(t))
	f := connect(t, a, "b")
	c, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c, "RFB 003.008\n")
	if ev := f.next(t); ev.Event != "open" {
		t.Fatalf("event %+v", ev)
	}
	// A second viewer connection gets its own tunnel.
	c2, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c2, "second")
	roundTrip(t, c, "first again")
	f.next(t)
	c.Close()
	c2.Close()
	for range 2 {
		if ev := f.next(t); ev.Event != "close" {
			t.Fatalf("event %+v", ev)
		}
	}
	eventually(t, "two closes in the log", func() bool {
		n := 0
		for _, e := range auditEvents(t, b) {
			if e.Event == "close" {
				n++
				if e.PeerName != "a" || e.PeerID != a.id() || !strings.HasPrefix(e.RemoteAddr, "127.0.0.1:") || e.BytesIn == 0 || e.BytesIn != e.BytesOut {
					t.Fatalf("close event %+v", e)
				}
			}
		}
		return n == 2
	})

	// Ending the request takes the listener down.
	f.cancel()
	if err := <-f.done; err != nil {
		t.Fatalf("connect returned %v", err)
	}
	eventually(t, "the listener closes", func() bool {
		c, err := net.Dial("tcp", f.addr)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}

// refused opens a viewer connection and expects the tunnel to be refused
// with code, and the target to log the reason.
func refused(t *testing.T, a, b *host, code, reason string) {
	t.Helper()
	f := connect(t, a, "b")
	defer f.cancel()
	c, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ev := f.next(t)
	if ev.Event != "error" || ev.Code != code {
		t.Fatalf("event %+v, want an error %s", ev, code)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the refused viewer connection stayed open")
	}
	if e := lastAudit(t, b); e.Event != "denied" || !strings.Contains(e.Reason, reason) {
		t.Fatalf("audit %+v, want denied with %q", e, reason)
	}
}

func TestRemoteRefusals(t *testing.T) {
	target := echo(t)
	a, b := remotePair(t, target)

	b.c.SetRemote(bg, node.RemoteSettings{Enabled: ptr(false)})
	refused(t, a, b, vfs.CodePeerUnauthorized, "remote control is off")

	b.c.SetRemote(bg, node.RemoteSettings{Enabled: ptr(true)})
	if err := b.c.DenyRemote(bg, "a"); err != nil {
		t.Fatal(err)
	}
	refused(t, a, b, vfs.CodePeerUnauthorized, "does not allow remote control from a")

	b.c.AllowRemote(bg, "a")
	b.c.SetRemote(bg, node.RemoteSettings{Networks: ptr([]string{})}) // back to Tailscale only
	refused(t, a, b, vfs.CodePeerUnauthorized, "only allowed over the remote-control networks")

	b.c.SetRemote(bg, node.RemoteSettings{Networks: ptr(loopbackNets), Target: ptr(freePort(t))})
	refused(t, a, b, vfs.CodePeerOffline, "screen service on this host is not running")

	// With everything in order again the same pair works.
	b.c.SetRemote(bg, node.RemoteSettings{Target: ptr(target)})
	f := connect(t, a, "b")
	c, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, "ok")
}

func TestRemoteControllerDialsOnlyItsNetworks(t *testing.T) {
	a, _ := remotePair(t, echo(t))
	a.c.SetRemote(bg, node.RemoteSettings{Networks: ptr([]string{})}) // Tailscale: b has only loopback addresses
	err := a.c.RemoteConnect(bg, "b", "", func(control.RemoteEvent) { t.Error("no event expected") })
	wantCode(t, err, vfs.CodeInvalidRequest)
}

func TestRemoteRemovalCutsTheSession(t *testing.T) {
	a, b := remotePair(t, echo(t))
	f := connect(t, a, "b")
	c, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, "before")
	f.next(t) // open
	if err := b.c.RemovePeer(bg, "a"); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("the viewer connection did not end cleanly: %v", err)
	}
	st, err := b.c.RemoteStatus(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Allow) != 1 || st.Allow[0].Valid {
		t.Fatalf("allow list after removal %+v", st.Allow)
	}
}

func TestRemoteSettingsValidation(t *testing.T) {
	a, b := newHost(t, "a"), newHost(t, "b")
	pair(t, a, b)
	wantCode(t, b.c.SetRemote(bg, node.RemoteSettings{Target: ptr("192.168.1.5:5900")}), vfs.CodeInvalidRequest)
	wantCode(t, b.c.SetRemote(bg, node.RemoteSettings{Networks: ptr([]string{"nope"})}), vfs.CodeInvalidRequest)
	wantCode(t, b.c.AllowRemote(bg, "local"), vfs.CodeInvalidRequest)
	wantCode(t, b.c.AllowRemote(bg, "nobody"), vfs.CodePeerNotFound)
	wantCode(t, b.c.DenyRemote(bg, "a"), vfs.CodePeerNotFound)
	wantCode(t, a.c.RemoteConnect(bg, "local", "", nil), vfs.CodeInvalidRequest)
	wantCode(t, a.c.RemoteConnect(bg, "b", "0.0.0.0:0", nil), vfs.CodeInvalidRequest)

	st, err := b.c.RemoteStatus(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Enabled || len(st.Allow) != 0 || len(st.Networks) != 2 {
		t.Fatalf("defaults %+v", st)
	}
	if isLinux := st.Target == ""; isLinux {
		// Linux has no screen service of its own: enabling needs a target.
		wantCode(t, b.c.SetRemote(bg, node.RemoteSettings{Enabled: ptr(true)}), vfs.CodeInvalidRequest)
	}
	if err := b.c.SetRemote(bg, node.RemoteSettings{Enabled: ptr(true), Target: ptr("127.0.0.1:5999")}); err != nil {
		t.Fatal(err)
	}
	st, _ = b.c.RemoteStatus(bg)
	if !st.Enabled || st.Target != "127.0.0.1:5999" || st.DefaultTarget {
		t.Fatalf("after set %+v", st)
	}
}
