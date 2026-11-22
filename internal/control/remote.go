package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"sugarintegration/internal/node"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/vfs"
)

// Remote control (remote-control spec 4.3). Settings change only here, never
// over the Peer API.

type (
	peerReq          struct{ Peer string }
	remoteConnectReq struct{ Peer, Listen string }
)

// RemoteEvent is one line of a connect stream: "listening" (Addr) first, then
// "open", "close" and "error" (Message) for each viewer connection.
type RemoteEvent struct {
	Event   string
	Addr    string `json:",omitempty"`
	Message string `json:",omitempty"`
	Code    string `json:",omitempty"`
}

func (s *server) remoteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Node.RemoteStatus())
}

func (s *server) remoteSettings(w http.ResponseWriter, r *http.Request) {
	var req node.RemoteSettings
	if decode(w, r, &req) {
		done(w, s.Node.SetRemote(req))
	}
}

func (s *server) remoteAllow(w http.ResponseWriter, r *http.Request) {
	var req peerReq
	if decode(w, r, &req) {
		done(w, s.Node.AllowRemote(req.Peer))
	}
}

func (s *server) remoteDeny(w http.ResponseWriter, r *http.Request) {
	var req peerReq
	if decode(w, r, &req) {
		done(w, s.Node.DenyRemote(req.Peer))
	}
}

// loopbackListen accepts host:port on a loopback address; port 0 picks one.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || (err == nil && ip.Unmap().IsLoopback())
}

// remoteConnect forwards a loopback listener to a peer's screen service for as
// long as this request stays open, so nothing outlives its caller.
func (s *server) remoteConnect(w http.ResponseWriter, r *http.Request) {
	var req remoteConnectReq
	if !decode(w, r, &req) {
		return
	}
	if req.Listen == "" {
		req.Listen = "127.0.0.1:0"
	}
	if !loopbackListen(req.Listen) {
		writeErr(w, vfs.E(vfs.CodeInvalidRequest, "the local address must be a loopback address such as 127.0.0.1:5901, not %q", req.Listen))
		return
	}
	id, local, err := s.Node.ResolvePeer(req.Peer)
	if err != nil {
		writeErr(w, err)
		return
	}
	if local {
		writeErr(w, vfs.E(vfs.CodeInvalidRequest, "connect to another host, not this one"))
		return
	}
	cl, err := s.Node.RemoteClient(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	ln, err := net.Listen("tcp", req.Listen)
	if err != nil {
		writeErr(w, vfs.E(vfs.CodeInvalidRequest, "cannot listen on %s: %v", req.Listen, err))
		return
	}
	defer ln.Close()

	// The request lasts as long as the forward: no read deadline, and the
	// body read to its end so that the server notices when the caller goes.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Time{})
	io.Copy(io.Discard, r.Body)
	ctx := r.Context()

	var mu sync.Mutex
	emit := func(ev RemoteEvent) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(ev)
		rc.Flush()
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	emit(RemoteEvent{Event: "listening", Addr: ln.Addr().String()})

	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			forward(ctx, cl, c, emit)
		}()
	}
	wg.Wait() // emit must not run once the handler has returned
}

func forward(ctx context.Context, cl *peerapi.Client, c net.Conn, emit func(RemoteEvent)) {
	defer c.Close()
	t, err := cl.Tunnel(ctx)
	if err != nil {
		emit(RemoteEvent{Event: "error", Message: err.Error(), Code: vfs.CodeOf(err)})
		return
	}
	defer t.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	emit(RemoteEvent{Event: "open"})
	up := make(chan struct{})
	go func() {
		defer close(up)
		io.Copy(t, c)
		if cw, ok := t.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	io.Copy(c, t)
	c.Close()
	t.Close()
	<-up
	emit(RemoteEvent{Event: "close"})
}

// ---- client ----

func (c *Client) RemoteStatus(ctx context.Context) (node.RemoteStatus, error) {
	var out node.RemoteStatus
	err := c.do(ctx, "GET", "/v1/remote", nil, nil, &out)
	return out, err
}

func (c *Client) SetRemote(ctx context.Context, s node.RemoteSettings) error {
	return c.do(ctx, "POST", "/v1/remote/settings", nil, s, nil)
}

func (c *Client) AllowRemote(ctx context.Context, peer string) error {
	return c.do(ctx, "POST", "/v1/remote/allow", nil, peerReq{Peer: peer}, nil)
}

func (c *Client) DenyRemote(ctx context.Context, peer string) error {
	return c.do(ctx, "POST", "/v1/remote/deny", nil, peerReq{Peer: peer}, nil)
}

// RemoteConnect forwards a loopback listener to peer's screen service until
// ctx ends, calling on for every event. It returns nil when ctx ends.
func (c *Client) RemoteConnect(ctx context.Context, peer, listen string, on func(RemoteEvent)) error {
	b, _ := json.Marshal(remoteConnectReq{Peer: peer, Listen: listen})
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/remote/connect", bytes.NewReader(b))
	if err != nil {
		return vfs.E(vfs.CodeInvalidRequest, "bad request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var ve vfs.Error
		if json.NewDecoder(io.LimitReader(resp.Body, maxReply)).Decode(&ve) != nil || ve.Code == "" {
			return vfs.E(vfs.CodeInternal, "control API returned HTTP %d", resp.StatusCode)
		}
		return &ve
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev RemoteEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			on(ev)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return vfs.E(vfs.CodeInternal, "the daemon ended the connection")
}
