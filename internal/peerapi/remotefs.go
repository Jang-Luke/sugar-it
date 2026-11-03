package peerapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"sugarintegration/internal/vfs"
)

// RemoteFS is a vfs.FS over the Peer API, with LocalFS's semantics and codes.
type RemoteFS struct {
	c     *Client
	share string
}

var _ vfs.FS = (*RemoteFS)(nil)

func (f *RemoteFS) path(op string) string { return "/v1/shares/" + url.PathEscape(f.share) + "/" + op }

func pathQuery(p string) url.Values { return url.Values{"path": {p}} }

func (f *RemoteFS) List(ctx context.Context, p string) ([]vfs.Entry, error) {
	var out []vfs.Entry
	if err := f.c.call(ctx, "GET", f.path("entries"), pathQuery(p), nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []vfs.Entry{}
	}
	return out, nil
}

func (f *RemoteFS) Stat(ctx context.Context, p string) (vfs.Entry, error) {
	var out vfs.Entry
	err := f.c.call(ctx, "GET", f.path("stat"), pathQuery(p), nil, &out)
	return out, err
}

// Open streams a file from offset, pinned to etag when given. An offset equal
// to the size yields an empty reader. Stream failures surface from Read as
// *vfs.Error: peer_offline for a dropped connection (so callers retry),
// canceled when ctx ends.
func (f *RemoteFS) Open(ctx context.Context, p string, offset int64, etag string) (io.ReadCloser, vfs.Entry, error) {
	if offset < 0 {
		return nil, vfs.Entry{}, vfs.E(vfs.CodeInvalidRequest, "negative offset")
	}
	h := http.Header{}
	if offset > 0 {
		h.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	if etag != "" {
		h.Set("If-Range", etag)
	}
	resp, err := f.c.send(ctx, "GET", f.path("file"), pathQuery(p), nil, h)
	if err != nil {
		return nil, vfs.Entry{}, err
	}
	e, err := decodeEntry(resp.Header.Get(entryHeader))
	if err != nil || resp.ContentLength < 0 {
		resp.Body.Close()
		return nil, vfs.Entry{}, vfs.E(vfs.CodeInternal, "peer sent a malformed file response")
	}
	return &bodyReader{ctx: ctx, rc: resp.Body, want: resp.ContentLength}, e, nil
}

type bodyReader struct {
	ctx  context.Context
	rc   io.ReadCloser
	want int64
	got  int64
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.got += int64(n)
	switch {
	case err == nil:
		return n, nil
	case err == io.EOF && b.got == b.want:
		return n, io.EOF
	case err == io.EOF: // the stream ended before the promised length
		return n, netErr(b.ctx, io.ErrUnexpectedEOF)
	}
	return n, netErr(b.ctx, err)
}

func (b *bodyReader) Close() error { return b.rc.Close() }

func (f *RemoteFS) Hash(ctx context.Context, p string) (string, error) {
	resp, err := f.c.send(ctx, "GET", f.path("hash"), pathQuery(p), nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var hb hashBody
	if err := json.NewDecoder(resp.Body).Decode(&hb); err != nil {
		return "", netErr(ctx, err) // no verdict arrived: the peer went away
	}
	if hb.Code != "" {
		if !knownCode(hb.Code) {
			return "", vfs.E(vfs.CodeInternal, "peer returned unknown error code")
		}
		return "", &vfs.Error{Code: hb.Code, Message: hb.Message}
	}
	if hb.SHA256 == "" {
		return "", vfs.E(vfs.CodeInternal, "peer sent an empty hash")
	}
	return hb.SHA256, nil
}

// recordReader remembers the first error r returned, so that a failure of the
// caller's source is reported as such rather than as a broken connection.
type recordReader struct {
	r   io.Reader
	err error
}

func (r *recordReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

func (f *RemoteFS) Put(ctx context.Context, p string, r io.Reader, o vfs.PutOptions) (vfs.Entry, error) {
	q := pathQuery(p)
	if o.Overwrite {
		q.Set("overwrite", "1")
	}
	if !o.ModTime.IsZero() {
		q.Set("mtime", strconv.FormatInt(o.ModTime.UnixNano(), 10))
	}
	if o.SHA256 != "" {
		q.Set("sha256", o.SHA256)
	}
	var body io.Reader = http.NoBody
	var rec *recordReader
	if r != nil {
		rec = &recordReader{r: r}
		body = rec
	}
	resp, err := f.c.send(ctx, "PUT", f.path("file"), q, body, http.Header{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		if rec != nil && rec.err != nil && ctx.Err() == nil {
			var ve *vfs.Error
			if errors.As(rec.err, &ve) {
				return vfs.Entry{}, ve
			}
			return vfs.Entry{}, vfs.E(vfs.CodeInternal, "source read: %v", rec.err)
		}
		return vfs.Entry{}, err
	}
	defer resp.Body.Close()
	var e vfs.Entry
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return vfs.Entry{}, netErr(ctx, err)
	}
	return e, nil
}

func (f *RemoteFS) Mkdir(ctx context.Context, p string) error {
	return f.c.call(ctx, "POST", f.path("mkdir"), nil, pathReq{p}, nil)
}

func (f *RemoteFS) Remove(ctx context.Context, p string, recursive bool) error {
	return f.c.call(ctx, "POST", f.path("remove"), nil, removeReq{p, recursive}, nil)
}

func (f *RemoteFS) Rename(ctx context.Context, from, to string) error {
	return f.c.call(ctx, "POST", f.path("rename"), nil, renameReq{from, to}, nil)
}
