package peerapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"sugarintegration/internal/vfs"
)

// TunnelPath is the remote-control tunnel: one HTTP/2 stream carries one TCP
// connection to the target's screen service (remote-control spec 4.1). The
// request body runs controller to target, the response body back.
const TunnelPath = "/v1/remote/tunnel"

const tunnelBuf = 32 << 10

func (s *server) tunnel(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor != 2 {
		writeErr(w, vfs.E(vfs.CodeInvalidRequest, "the remote tunnel needs HTTP/2"))
		return
	}
	sess, err := s.b.OpenTunnel(r.Context(), caller(r), r.RemoteAddr)
	if err != nil {
		writeErr(w, err)
		return
	}
	conn := sess.Conn
	// A removed caller or a closing server ends the request's context; closing
	// the connection unblocks both directions.
	stop := context.AfterFunc(r.Context(), func() { conn.Close() })
	defer stop()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		sess.Finish(0, 0)
		return
	}

	var in atomic.Int64
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		n, _ := io.CopyBuffer(conn, r.Body, make([]byte, tunnelBuf))
		in.Add(n)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()

	var out int64
	buf := make([]byte, tunnelBuf)
	for {
		k, rerr := conn.Read(buf)
		if k > 0 {
			if _, werr := w.Write(buf[:k]); werr != nil {
				break
			}
			out += int64(k)
			if rc.Flush() != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	// The body must not be read once the handler returns: closing it wakes
	// a blocked read so the upload goroutine finishes first.
	conn.Close()
	r.Body.Close()
	<-upDone
	sess.Finish(in.Load(), out)
}

// Tunnel opens a tunnel to the peer's screen service. It lives until ctx ends
// or Close is called.
func (c *Client) Tunnel(ctx context.Context) (io.ReadWriteCloser, error) {
	pr, pw := io.Pipe()
	resp, err := c.send(ctx, "POST", TunnelPath, nil, pr, nil)
	if err != nil {
		pw.Close()
		return nil, err
	}
	if resp.ProtoMajor != 2 {
		resp.Body.Close()
		pw.Close()
		return nil, vfs.E(vfs.CodeInternal, "the peer did not answer over HTTP/2")
	}
	t := &tunnelConn{r: resp.Body, w: pw}
	// While it waits on the request body the transport does not watch ctx, so
	// the tunnel closes itself; closing the response body resets the stream.
	stop := context.AfterFunc(ctx, func() { t.Close() })
	t.mu.Lock()
	t.stop = stop
	t.mu.Unlock()
	return t, nil
}

type tunnelConn struct {
	r    io.ReadCloser
	w    *io.PipeWriter
	once sync.Once
	mu   sync.Mutex
	stop func() bool // set right after the AfterFunc, which may already run
}

func (t *tunnelConn) Read(b []byte) (int, error) { return t.r.Read(b) }

func (t *tunnelConn) Write(b []byte) (int, error) {
	n, err := t.w.Write(b)
	if errors.Is(err, io.ErrClosedPipe) {
		err = net.ErrClosed
	}
	return n, err
}

// CloseWrite ends the controller-to-target direction only.
func (t *tunnelConn) CloseWrite() error { return t.w.Close() }

func (t *tunnelConn) Close() error {
	t.once.Do(func() {
		t.mu.Lock()
		stop := t.stop
		t.mu.Unlock()
		if stop != nil {
			stop()
		}
		t.w.Close()
		t.r.Close()
	})
	return nil
}
