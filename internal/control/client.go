package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/node"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

const (
	discoverTimeout = 2 * time.Second
	maxReply        = 64 << 20
)

// Client talks to the daemon's Control API.
type Client struct {
	base, token, davAddr string
	hc                   *http.Client
}

// Discover finds the daemon of configDir through its runtime.json and the
// control token. A missing or stale runtime.json (nothing answers behind it)
// is ErrUnavailable. configDir "" means the default.
func Discover(configDir string) (*Client, error) {
	if configDir == "" {
		d, err := config.DefaultDir()
		if err != nil {
			return nil, err
		}
		configDir = d
	}
	rt, err := readRuntime(configDir)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	// runtime.json exists only while a daemon, which created the token, runs.
	tok, err := config.ControlToken(configDir)
	if err != nil {
		return nil, fmt.Errorf("%w (cannot read the control token: %v)", ErrUnavailable, err)
	}
	c := &Client{base: "http://" + rt.ControlAddr, token: tok, davAddr: rt.DAVAddr, hc: &http.Client{}}
	ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
	defer cancel()
	if _, err := c.Status(ctx); err != nil {
		if errors.Is(err, ErrUnavailable) {
			return nil, fmt.Errorf("%w (stale %s)", ErrUnavailable, RuntimeFile)
		}
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	return c, nil
}

// Token is the control token, also the WebDAV password.
func (c *Client) Token() string { return c.token }

// DAVAddr is the address the WebDAV listener is bound to.
func (c *Client) DAVAddr() string { return c.davAddr }

// do sends a JSON request and decodes the reply into out, if non-nil.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return vfs.E(vfs.CodeInternal, "encode: %v", err)
		}
		body = bytes.NewReader(b)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return vfs.E(vfs.CodeInvalidRequest, "bad request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return vfs.E(vfs.CodeCanceled, "canceled")
		}
		// The text of a transport error names the URL, never the header.
		return fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	if err != nil {
		return fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	if resp.StatusCode >= 400 {
		var ve vfs.Error
		if json.Unmarshal(b, &ve) != nil || ve.Code == "" {
			return vfs.E(vfs.CodeInternal, "control API returned HTTP %d", resp.StatusCode)
		}
		return &ve
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return vfs.E(vfs.CodeInternal, "bad reply from the control API: %v", err)
	}
	return nil
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var out Status
	err := c.do(ctx, "GET", "/v1/status", nil, nil, &out)
	return out, err
}

// Invite makes an invitation valid for ttl (0 means the default, 10 minutes).
func (c *Client) Invite(ctx context.Context, ttl time.Duration) (string, error) {
	var out inviteResp
	err := c.do(ctx, "POST", "/v1/invite", nil, inviteReq{int64(ttl / time.Second)}, &out)
	return out.Invite, err
}

func (c *Client) JoinPeer(ctx context.Context, invite string) (node.PeerStatus, error) {
	var out node.PeerStatus
	err := c.do(ctx, "POST", "/v1/peers/join", nil, joinReq{invite}, &out)
	return out, err
}

func (c *Client) RemovePeer(ctx context.Context, name string) error {
	return c.do(ctx, "POST", "/v1/peers/remove", nil, nameReq{name}, nil)
}

func (c *Client) RenameSelf(ctx context.Context, newName string) error {
	return c.do(ctx, "POST", "/v1/self/rename", nil, nameReq{newName}, nil)
}

// ListShares lists the shares of peer ("" or "local" for this host).
func (c *Client) ListShares(ctx context.Context, peer string) ([]peerapi.ShareInfo, error) {
	var out []peerapi.ShareInfo
	err := c.do(ctx, "GET", "/v1/shares", url.Values{"peer": {peer}}, nil, &out)
	return out, err
}

func (c *Client) AddShare(ctx context.Context, s shares.Spec) error {
	return c.do(ctx, "POST", "/v1/shares/add", nil, s, nil)
}

func (c *Client) RemoveShare(ctx context.Context, name string) error {
	return c.do(ctx, "POST", "/v1/shares/remove", nil, nameReq{name}, nil)
}

func (c *Client) List(ctx context.Context, t Target) ([]vfs.Entry, error) {
	var out []vfs.Entry
	err := c.do(ctx, "POST", "/v1/fs/list", nil, targetReq{t}, &out)
	return out, err
}

func (c *Client) Mkdir(ctx context.Context, t Target) error {
	return c.do(ctx, "POST", "/v1/fs/mkdir", nil, targetReq{t}, nil)
}

func (c *Client) Remove(ctx context.Context, t Target, recursive bool) error {
	return c.do(ctx, "POST", "/v1/fs/remove", nil, removeReq{t, recursive}, nil)
}

func (c *Client) Rename(ctx context.Context, t Target, newName string) error {
	return c.do(ctx, "POST", "/v1/fs/rename", nil, renameReq{t, newName}, nil)
}

// StartJob starts a copy or move. The destination host runs it.
func (c *Client) StartJob(ctx context.Context, r TransferRequest) (transfer.Job, error) {
	var out transfer.Job
	err := c.do(ctx, "POST", "/v1/jobs", nil, r, &out)
	return out, err
}

// Jobs lists this host's jobs and the jobs of online peers that this host
// requested. An unreachable peer is left out, not an error.
func (c *Client) Jobs(ctx context.Context) ([]transfer.Job, error) {
	var out []transfer.Job
	err := c.do(ctx, "GET", "/v1/jobs", nil, nil, &out)
	return out, err
}

func jobPath(host, id, suffix string) string {
	return "/v1/jobs/" + url.PathEscape(host) + "/" + url.PathEscape(id) + suffix
}

// Job returns one job. host is the member ID of the host it runs on (Job.Host).
func (c *Client) Job(ctx context.Context, host, id string) (transfer.Job, error) {
	var out transfer.Job
	err := c.do(ctx, "GET", jobPath(host, id, ""), nil, nil, &out)
	return out, err
}

func (c *Client) CancelJob(ctx context.Context, host, id string) error {
	return c.do(ctx, "POST", jobPath(host, id, "/cancel"), nil, nil, nil)
}

func (c *Client) RetryJob(ctx context.Context, host, id string) error {
	return c.do(ctx, "POST", jobPath(host, id, "/retry"), nil, nil, nil)
}
