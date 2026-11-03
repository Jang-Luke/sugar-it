package peerapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"sugarintegration/internal/identity"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

const (
	dialTimeout    = 5 * time.Second
	headerTimeout  = 30 * time.Second
	syntheticHost  = "peer.invalid" // never resolved: dialing uses the member's addresses
	maxErrBodySize = 64 << 10
)

// Client talks to one peer, whose certificate is pinned by ID.
type Client struct {
	id     *identity.Identity
	peerID string
	hc     *http.Client
	base   string
	cfg    *tls.Config
	dial   func(ctx context.Context, addr string) (net.Conn, error) // tests observe it

	mu    sync.Mutex
	addrs []string
	pref  int // index of the last address that worked
}

func defaultDial(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout, KeepAliveConfig: net.KeepAliveConfig{
		Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3}}
	return d.DialContext(ctx, "tcp", addr)
}

// clientTLSConfig pins the server to peerID. The check is in VerifyConnection,
// not VerifyPeerCertificate, because only the former also runs when a session
// is resumed. The client keeps no session cache, so it never resumes anyway.
func clientTLSConfig(id *identity.Identity, peerID string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // trust is the pinned ID below, not a CA
		Certificates:       []tls.Certificate{id.Cert},
		NextProtos:         []string{"h2", "http/1.1"},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || identity.IDOf(cs.PeerCertificates[0]) != peerID {
				return errors.New("peer identity does not match the pinned ID")
			}
			return nil
		},
	}
}

func NewClient(id *identity.Identity, peerID string, addrs []string) *Client {
	c := &Client{id: id, peerID: peerID, addrs: append([]string(nil), addrs...),
		base: "https://" + syntheticHost, cfg: clientTLSConfig(id, peerID), dial: defaultDial}
	t := &http.Transport{
		DialTLSContext:        c.dialTLS,
		TLSClientConfig:       c.cfg,
		ForceAttemptHTTP2:     true, // a custom dialer would otherwise disable h2
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       time.Minute,
		HTTP2:                 &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 10 * time.Second},
	}
	c.hc = &http.Client{Transport: t} // no overall timeout: transfers are long
	return c
}

// SetAddrs replaces the addresses to try.
func (c *Client) SetAddrs(addrs []string) {
	c.mu.Lock()
	changed := len(addrs) != len(c.addrs)
	for i := 0; !changed && i < len(addrs); i++ {
		changed = addrs[i] != c.addrs[i]
	}
	c.addrs, c.pref = append([]string(nil), addrs...), 0
	c.mu.Unlock()
	if changed {
		c.hc.CloseIdleConnections()
	}
}

// dialTLS connects and handshakes with each address in turn, starting at the
// last one that worked. An address held by a different host fails the pin
// and is skipped like a dead one.
func (c *Client) dialTLS(ctx context.Context, _, _ string) (net.Conn, error) {
	c.mu.Lock()
	addrs, pref := append([]string(nil), c.addrs...), c.pref
	c.mu.Unlock()
	if len(addrs) == 0 {
		return nil, errors.New("no addresses known")
	}
	var last error
	for k := range addrs {
		i := (pref + k) % len(addrs)
		conn, err := c.dialOne(ctx, addrs[i])
		if err == nil {
			c.mu.Lock()
			c.pref = i
			c.mu.Unlock()
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		last = err
	}
	return nil, last
}

func (c *Client) dialOne(ctx context.Context, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	raw, err := c.dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, c.cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return tc, nil
}

// netErr turns a transport failure into a spec error.
func netErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return vfs.E(vfs.CodeCanceled, "canceled")
	}
	var ve *vfs.Error
	if errors.As(err, &ve) {
		return ve
	}
	return vfs.E(vfs.CodePeerOffline, "peer unreachable: %v", err)
}

// remoteErr decodes an error response.
func remoteErr(ctx context.Context, resp *http.Response) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
	if err != nil {
		return netErr(ctx, err)
	}
	var ve vfs.Error
	if json.Unmarshal(b, &ve) != nil || !knownCode(ve.Code) {
		return vfs.E(vfs.CodeInternal, "peer returned HTTP %d", resp.StatusCode)
	}
	return &ve
}

// send performs a request and returns the response if it succeeded. Failure
// responses are decoded into *vfs.Error and their bodies closed.
func (c *Client) send(ctx context.Context, method, path string, q url.Values, body io.Reader, hdr http.Header) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, vfs.E(vfs.CodeInvalidRequest, "bad request: %v", err)
	}
	if body != nil && body != http.NoBody {
		if _, ok := body.(*bytes.Reader); !ok {
			req.ContentLength = -1
		}
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, netErr(ctx, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, remoteErr(ctx, resp)
	}
	return resp, nil
}

// call sends a JSON request and decodes a JSON reply into out (if non-nil).
func (c *Client) call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return vfs.E(vfs.CodeInternal, "encode: %v", err)
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.send(ctx, method, path, q, body, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return netErr(ctx, err) // a truncated reply means the peer went away
	}
	return nil
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.call(ctx, "GET", "/v1/info", nil, nil, &out)
	return out, err
}

func (c *Client) Pair(ctx context.Context, r PairRequest) (PairResponse, error) {
	var out PairResponse
	err := c.call(ctx, "POST", "/v1/pair", nil, r, &out)
	return out, err
}

func (c *Client) SyncMembers(ctx context.Context, m []mesh.Member) ([]mesh.Member, error) {
	if m == nil {
		m = []mesh.Member{}
	}
	var out []mesh.Member
	err := c.call(ctx, "POST", "/v1/members/sync", nil, m, &out)
	return out, err
}

func (c *Client) Shares(ctx context.Context) ([]ShareInfo, error) {
	var out []ShareInfo
	err := c.call(ctx, "GET", "/v1/shares", nil, nil, &out)
	return out, err
}

func (c *Client) FS(share string) vfs.FS { return &RemoteFS{c: c, share: share} }

func (c *Client) SubmitJob(ctx context.Context, s transfer.Spec) (transfer.Job, error) {
	var out transfer.Job
	err := c.call(ctx, "POST", "/v1/jobs", nil, s, &out)
	return out, err
}

func (c *Client) ListJobs(ctx context.Context, origin string) ([]transfer.Job, error) {
	var out []transfer.Job
	err := c.call(ctx, "GET", "/v1/jobs", url.Values{"origin": {origin}}, nil, &out)
	return out, err
}

func (c *Client) CancelJob(ctx context.Context, id string) error {
	return c.call(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/cancel", nil, nil, nil)
}

func (c *Client) RetryJob(ctx context.Context, id string) error {
	return c.call(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/retry", nil, nil, nil)
}
