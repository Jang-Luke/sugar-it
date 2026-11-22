package peerapi

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"sugarintegration/internal/remote"
	"sugarintegration/internal/vfs"
)

func (b *fakeBackend) OpenTunnel(ctx context.Context, callerID, remoteAddr string) (*remote.Session, error) {
	b.mu.Lock()
	target := b.tunnelTarget
	end := b.tunnelEnd
	if target != "" {
		b.tunnelCaller, b.tunnelFrom = callerID, remoteAddr
	}
	b.mu.Unlock()
	if target == "" {
		return nil, vfs.E(vfs.CodePeerUnauthorized, "remote control is off on this host")
	}
	conn, err := net.Dial("tcp", target)
	if err != nil {
		return nil, vfs.E(vfs.CodePeerOffline, "not running")
	}
	return remote.NewSession(conn, func(in, out int64) {
		if end != nil {
			end <- [2]int64{in, out}
		}
	}), nil
}

func (b *fakeBackend) setTunnel(target string) chan [2]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tunnelTarget, b.tunnelEnd = target, make(chan [2]int64, 1)
	return b.tunnelEnd
}

func (b *fakeBackend) tunnelSeen() (caller, from string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tunnelCaller, b.tunnelFrom
}

// echoServer echoes each connection; a connection may also ask it to close
// by sending "bye".
func echoServer(t *testing.T) (addr string, conns chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	conns = make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if bytes.Equal(buf[:n], []byte("bye")) {
							return
						}
						c.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), conns
}

func TestTunnelRoundTrip(t *testing.T) {
	e := start(t, "linux")
	addr, _ := echoServer(t)
	end := e.b.setTunnel(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tc, err := e.c.Tunnel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Several exchanges prove each direction is flushed as it goes, not at the end.
	total := 0
	for _, msg := range []string{"hello", "RFB 003.008\n", string(bytes.Repeat([]byte("x"), 100000))} {
		if _, err := tc.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(tc, got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != msg {
			t.Fatalf("echo mismatch for a %d-byte message", len(msg))
		}
		total += len(msg)
	}
	caller, from := e.b.tunnelSeen()
	if caller != e.cliID.ID {
		t.Errorf("caller %q", caller)
	}
	if h, _, _ := net.SplitHostPort(from); h != "127.0.0.1" {
		t.Errorf("remote addr %q", from)
	}
	// The target closing ends the tunnel for the controller.
	tc.Write([]byte("bye"))
	if _, err := io.ReadAll(tc); err != nil {
		t.Fatalf("after the target closed: %v", err)
	}
	tc.Close()
	select {
	case got := <-end:
		if got[0] != int64(total+3) || got[1] != int64(total) {
			t.Errorf("bytes in/out %v, want %d/%d", got, total+3, total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session never finished")
	}
}

func TestTunnelControllerCloseEndsSession(t *testing.T) {
	e := start(t, "linux")
	addr, conns := echoServer(t)
	end := e.b.setTunnel(addr)
	tc, err := e.c.Tunnel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := <-conns
	tc.Close()
	select {
	case <-end:
	case <-time.After(5 * time.Second):
		t.Fatal("session never finished after the controller closed")
	}
	target.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := target.Read(make([]byte, 1)); err == nil {
		t.Fatal("the target connection is still open")
	}
}

func TestTunnelContextEndsSession(t *testing.T) {
	e := start(t, "linux")
	addr, _ := echoServer(t)
	end := e.b.setTunnel(addr)
	ctx, cancel := context.WithCancel(context.Background())
	tc, err := e.c.Tunnel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	cancel()
	select {
	case <-end:
	case <-time.After(5 * time.Second):
		t.Fatal("session never finished after the context ended")
	}
}

func TestTunnelRevokedCallerIsCut(t *testing.T) {
	e := start(t, "linux")
	addr, _ := echoServer(t)
	end := e.b.setTunnel(addr)
	tc, err := e.c.Tunnel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	e.b.revoke(e.cliID.ID)
	select {
	case <-end:
	case <-time.After(5 * time.Second):
		t.Fatal("a removed member's tunnel stayed open")
	}
	io.ReadAll(tc) // an error or EOF: the stream must end rather than hang
}

func TestTunnelRefusedAndNonMember(t *testing.T) {
	e := start(t, "linux")
	_, err := e.c.Tunnel(context.Background())
	wantCode(t, err, vfs.CodePeerUnauthorized)

	stranger := NewClient(newID(t), e.srvID.ID, []string{e.addr})
	addr, _ := echoServer(t)
	e.b.setTunnel(addr)
	_, err = stranger.Tunnel(context.Background())
	wantCode(t, err, vfs.CodePeerUnauthorized)
	if c, _ := e.b.tunnelSeen(); c != "" {
		t.Fatal("a stranger reached OpenTunnel")
	}
}
