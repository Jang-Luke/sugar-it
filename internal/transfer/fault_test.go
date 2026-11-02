package transfer

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"sugarintegration/internal/shares"
	"sugarintegration/internal/vfs"
)

// faultFS wraps a source FS: streams drop with peer_offline after dropAfter
// bytes, hook runs once per stream when hookAt bytes were delivered, and
// hash can be overridden.
type faultFS struct {
	vfs.FS
	mu        sync.Mutex
	dropAfter int64 // <0: never
	hookAt    int64 // <0: never
	hook      func()
	hash      string
	offsets   []int64
	dropCode  string // error code of a drop; default peer_offline
	hookPath  string // if set, hook only fires on streams of this path
	opened    []string
	calls     []string             // "open p", "hash p", "remove p", "rename p"
	onRemove  func(p string) error // if set and non-nil result, Remove fails with it
}

func (f *faultFS) record(op, p string) {
	f.mu.Lock()
	f.calls = append(f.calls, op+" "+p)
	f.mu.Unlock()
}

// callLog returns a copy of the recorded calls.
func (f *faultFS) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// openedPaths returns a copy of the paths passed to Open.
func (f *faultFS) openedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}

func (f *faultFS) setDrop(n int64) {
	f.mu.Lock()
	f.dropAfter = n
	f.mu.Unlock()
}

func (f *faultFS) Remove(ctx context.Context, p string, recursive bool) error {
	f.record("remove", p)
	f.mu.Lock()
	on := f.onRemove
	f.mu.Unlock()
	if on != nil {
		if err := on(p); err != nil {
			return err
		}
	}
	return f.FS.Remove(ctx, p, recursive)
}

func (f *faultFS) Rename(ctx context.Context, from, to string) error {
	f.record("rename", from)
	return f.FS.Rename(ctx, from, to)
}

func newFault(inner vfs.FS) *faultFS { return &faultFS{FS: inner, dropAfter: -1, hookAt: -1} }

func (f *faultFS) Open(ctx context.Context, p string, off int64, etag string) (io.ReadCloser, vfs.Entry, error) {
	f.record("open", p)
	rc, e, err := f.FS.Open(ctx, p, off, etag)
	if err != nil {
		return nil, e, err
	}
	f.mu.Lock()
	f.offsets = append(f.offsets, off)
	f.opened = append(f.opened, p)
	drop, hookAt, hook := f.dropAfter, f.hookAt, f.hook
	if f.hookPath != "" && f.hookPath != p {
		hookAt, hook = -1, nil
	}
	f.mu.Unlock()
	return &faultReader{rc: rc, drop: drop, dropCode: f.dropCode, hookAt: hookAt, hook: hook}, e, nil
}

func (f *faultFS) Hash(ctx context.Context, p string) (string, error) {
	f.record("hash", p)
	if f.hash != "" {
		return f.hash, nil
	}
	return f.FS.Hash(ctx, p)
}

type faultReader struct {
	rc       io.ReadCloser
	drop     int64
	dropCode string
	hookAt   int64
	hook     func()
	n        int64
	hooked   bool
}

func (r *faultReader) Read(b []byte) (int, error) {
	if r.hookAt >= 0 && !r.hooked && r.n >= r.hookAt && r.hook != nil {
		r.hooked = true
		r.hook()
	}
	if r.drop >= 0 {
		if r.n >= r.drop {
			code := r.dropCode
			if code == "" {
				code = vfs.CodePeerOffline
			}
			return 0, vfs.E(code, "connection dropped")
		}
		if rem := r.drop - r.n; int64(len(b)) > rem {
			b = b[:rem]
		}
	}
	if r.hookAt >= 0 && !r.hooked && int64(len(b)) > r.hookAt-r.n && r.hookAt > r.n {
		b = b[:r.hookAt-r.n]
	}
	n, err := r.rc.Read(b)
	r.n += int64(n)
	return n, err
}

func (r *faultReader) Close() error { return r.rc.Close() }

// ciDest is a destination whose Stat ignores case, like Windows or macOS.
type ciDest struct{ *shares.LocalFS }

func (c ciDest) Stat(ctx context.Context, p string) (vfs.Entry, error) {
	if e, err := c.LocalFS.Stat(ctx, p); err == nil {
		return e, nil
	}
	i := strings.LastIndex(p, "/")
	dir, base := "", p
	if i >= 0 {
		dir, base = p[:i], p[i+1:]
	}
	es, err := c.LocalFS.List(ctx, dir)
	if err != nil {
		return vfs.Entry{}, err
	}
	for _, e := range es {
		if strings.EqualFold(e.Name, base) {
			return e, nil
		}
	}
	return vfs.Entry{}, vfs.E(vfs.CodeNotFound, "not found")
}

// failDest fails CommitPart and/or OpenPart writes.
type failDest struct {
	*shares.LocalFS
	commitErr error
	partFile  string // if set, OpenPart returns this file (e.g. /dev/full)
}

func (d failDest) CommitPart(key, dst string, mt time.Time, mode uint32) error {
	if d.commitErr != nil {
		return d.commitErr
	}
	return d.LocalFS.CommitPart(key, dst, mt, mode)
}

func (d failDest) OpenPart(key string) (*os.File, int64, error) {
	if d.partFile == "" {
		return d.LocalFS.OpenPart(key)
	}
	f, err := os.OpenFile(d.partFile, os.O_RDWR, 0)
	return f, 0, err
}
