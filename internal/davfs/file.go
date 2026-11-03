package davfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"time"

	"golang.org/x/net/webdav"

	"sugarintegration/internal/vfs"
)

// info is the os.FileInfo of a tree node. It also answers the content type
// and ETag questions itself so that a PROPFIND never opens a file.
type info struct {
	name string
	size int64
	dir  bool
	mt   time.Time
	etag string
}

func (i info) Name() string { return i.name }
func (i info) Size() int64  { return i.size }
func (i info) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i info) ModTime() time.Time { return i.mt }
func (i info) IsDir() bool        { return i.dir }
func (i info) Sys() any           { return nil }

func (i info) ContentType(ctx context.Context) (string, error) {
	if i.dir {
		return "", webdav.ErrNotImplemented
	}
	if t := mime.TypeByExtension(path.Ext(i.name)); t != "" {
		return t, nil
	}
	return "application/octet-stream", nil
}

func (i info) ETag(ctx context.Context) (string, error) {
	if i.etag == "" {
		return "", webdav.ErrNotImplemented
	}
	return `"` + i.etag + `"`, nil
}

func infoOf(e vfs.Entry) info {
	return info{name: e.Name, size: e.Size, dir: e.Type == vfs.TypeDir, mt: e.ModTime, etag: e.ETag}
}

var errNotSupported = errors.New("operation not supported on this WebDAV node")

// dirFile is an open directory; its listing is fetched on first use.
type dirFile struct {
	self info
	load func() ([]os.FileInfo, error)
	rest []os.FileInfo
	got  bool
}

func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Read([]byte) (int, error)       { return 0, errNotSupported }
func (d *dirFile) Write([]byte) (int, error)      { return 0, errNotSupported }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, errNotSupported }
func (d *dirFile) Stat() (os.FileInfo, error)     { return d.self, nil }

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if !d.got {
		l, err := d.load()
		if err != nil {
			return nil, err
		}
		d.rest, d.got = l, true
	}
	if count <= 0 {
		out := d.rest
		d.rest = nil
		return out, nil
	}
	if len(d.rest) == 0 {
		return nil, io.EOF
	}
	n := min(count, len(d.rest))
	out := d.rest[:n]
	d.rest = d.rest[n:]
	return out, nil
}

// readFile streams a remote file. It opens the stream lazily and re-opens at
// an offset for Seek, pinned to the ETag it first saw: if the source changed
// meanwhile the read fails instead of mixing old and new bytes.
type readFile struct {
	ctx  context.Context
	fs   vfs.FS
	path string
	self info // from the listing until the first open, then fresh

	opened bool
	etag   string
	size   int64
	pos    int64
	rc     io.ReadCloser
	rcPos  int64
}

func (f *readFile) ensure() error {
	if f.opened {
		return nil
	}
	rc, e, err := f.fs.Open(f.ctx, f.path, 0, "")
	if err != nil {
		return err
	}
	f.rc, f.rcPos, f.opened = rc, 0, true
	f.etag, f.size = e.ETag, e.Size
	f.self = infoOf(e)
	return nil
}

func (f *readFile) Stat() (os.FileInfo, error) { return f.self, nil }
func (f *readFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, errNotSupported
}
func (f *readFile) Write([]byte) (int, error) { return 0, errNotSupported }

func (f *readFile) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = f.pos + off
	case io.SeekEnd:
		if err := f.ensure(); err != nil {
			return 0, err
		}
		abs = f.size + off
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative seek position")
	}
	f.pos = abs
	return abs, nil
}

func (f *readFile) Read(b []byte) (int, error) {
	if err := f.ensure(); err != nil {
		return 0, err
	}
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if f.rc == nil || f.rcPos != f.pos {
		if f.rc != nil {
			f.rc.Close()
			f.rc = nil
		}
		rc, e, err := f.fs.Open(f.ctx, f.path, f.pos, f.etag)
		if err != nil {
			return 0, err
		}
		if e.ETag != f.etag {
			rc.Close()
			return 0, vfs.E(vfs.CodeChanged, "the file changed while it was being read")
		}
		f.rc, f.rcPos = rc, f.pos
	}
	n, err := f.rc.Read(b)
	f.pos += int64(n)
	f.rcPos += int64(n)
	if errors.Is(err, io.EOF) && f.pos < f.size {
		err = io.ErrUnexpectedEOF // shorter than announced
	}
	return n, err
}

func (f *readFile) Close() error {
	if f.rc != nil {
		err := f.rc.Close()
		f.rc = nil
		return err
	}
	return nil
}

// bodyKey carries the upload body's health from the handler to the file.
type bodyKey struct{}

// bodyState records whether a PUT body was read completely and without
// error. x/net's handler closes the file even when the copy failed, so
// Close needs this to avoid publishing a truncated upload.
type bodyState struct {
	want, n int64
	failed  bool
}

func (s *bodyState) broken() bool { return s.failed || (s.want >= 0 && s.n != s.want) }

type trackBody struct {
	io.ReadCloser
	st *bodyState
}

func (t *trackBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.st.n += int64(n)
	if err != nil && err != io.EOF {
		t.st.failed = true
	}
	return n, err
}

// writeFile spools an upload to a private temp file outside any share and
// hands it to Put on Close, so the share never sees a partial file.
type writeFile struct {
	fsys      *fileSystem
	ctx       context.Context
	l         loc
	target    vfs.FS
	name      string
	overwrite bool

	spool  *os.File
	n      int64
	failed bool
	closed bool
}

func (w *writeFile) Read([]byte) (int, error)           { return 0, errNotSupported }
func (w *writeFile) Readdir(int) ([]os.FileInfo, error) { return nil, errNotSupported }
func (w *writeFile) Seek(int64, int) (int64, error)     { return 0, errNotSupported }
func (w *writeFile) Stat() (os.FileInfo, error) {
	return info{name: path.Base(w.name), size: w.n, mt: time.Now()}, nil
}

func (w *writeFile) Write(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	n, err := w.spool.Write(p)
	w.n += int64(n)
	if err != nil {
		w.failed = true
	}
	return n, err
}

func (w *writeFile) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer func() {
		w.spool.Close()
		os.Remove(w.spool.Name())
	}()
	// Whatever happens next, the directory may have changed.
	defer w.fsys.changed(w.l)
	if w.failed {
		return errors.New("upload spool failed")
	}
	if st, _ := w.ctx.Value(bodyKey{}).(*bodyState); st != nil && st.broken() {
		return io.ErrUnexpectedEOF
	}
	if _, err := w.spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := w.target.Put(w.ctx, w.l.rest, w.spool, vfs.PutOptions{Overwrite: w.overwrite})
	return w.fsys.pathErr(w.ctx, "put", w.name, err)
}
