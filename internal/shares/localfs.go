// Package shares implements the os.Root-confined local share filesystem.
package shares

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"sugarintegration/internal/vfs"
)

type Spec struct {
	Name     string
	Path     string
	ReadOnly bool
}

// LocalFS serves one share directory. All access goes through os.Root, so
// ".." and symlinks leading outside the share cannot be followed.
type LocalFS struct {
	root     *os.Root
	readOnly bool
	syncDir  func(dir string) error // default syncDirIn; tests observe it
}

var _ vfs.FS = (*LocalFS)(nil)

func Open(s Spec) (*LocalFS, error) {
	r, err := os.OpenRoot(s.Path)
	if err != nil {
		return nil, mapErr(err)
	}
	st, err := r.Stat(".")
	if err != nil || !st.IsDir() {
		r.Close()
		return nil, vfs.E(vfs.CodeInvalidPath, "share path is not a directory")
	}
	l := &LocalFS{root: r, readOnly: s.ReadOnly}
	l.syncDir = func(dir string) error { return syncDirIn(r, dir) }
	return l, nil
}

// SyncParent makes the name p in its directory durable: a rename or a new file
// is not committed until its directory has been synced. A no-op on Windows.
func (l *LocalFS) SyncParent(p string) error {
	c, err := l.cleanPath(p)
	if err != nil {
		return err
	}
	return l.syncParent(c)
}

func (l *LocalFS) syncParent(clean string) error {
	dir := "."
	if i := strings.LastIndexByte(clean, '/'); i >= 0 {
		dir = clean[:i]
	}
	err := l.syncDir(dir)
	// Network and FAT-style mounts reject directory fsync. Nothing more can be
	// done for such a share, and failing would stop every commit to it.
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return mapErr(err)
}

func (l *LocalFS) Close() error   { return l.root.Close() }
func (l *LocalFS) ReadOnly() bool { return l.readOnly }

func (l *LocalFS) writable() error {
	if l.readOnly {
		return vfs.E(vfs.CodeReadOnly, "share is read-only")
	}
	return nil
}

// mapErr converts OS errors into spec error codes.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ve *vfs.Error
	switch {
	case errors.As(err, &ve):
		return err
	case isDiskFull(err):
		return vfs.E(vfs.CodeDiskFull, "no space left on device")
	case strings.Contains(err.Error(), "path escapes from parent"):
		return vfs.E(vfs.CodeInvalidPath, "path escapes the share")
	case errors.Is(err, fs.ErrNotExist):
		return vfs.E(vfs.CodeNotFound, "not found")
	case errors.Is(err, syscall.ENOTEMPTY):
		return vfs.E(vfs.CodeNotEmpty, "directory not empty")
	case errors.Is(err, fs.ErrExist):
		return vfs.E(vfs.CodeExists, "already exists")
	case errors.Is(err, fs.ErrPermission):
		return vfs.E(vfs.CodeInternal, "permission denied")
	case errors.Is(err, syscall.ENOTDIR):
		return vfs.E(vfs.CodeNotFound, "not a directory")
	}
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		return vfs.E(vfs.CodeInternal, "%s: %v", pe.Op, pe.Err)
	case errors.As(err, &le):
		return vfs.E(vfs.CodeInternal, "%s: %v", le.Op, le.Err)
	}
	return vfs.E(vfs.CodeInternal, "%v", err)
}

// noLinks rejects a clean path whose intermediate components are symlinks,
// so nothing is ever reached through a link (inside the share or not).
// Missing components are left for the operation itself to report.
func (l *LocalFS) noLinks(clean string) error {
	for i := 0; i < len(clean); i++ {
		if clean[i] != '/' {
			continue
		}
		fi, err := l.root.Lstat(clean[:i])
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return mapErr(err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return vfs.E(vfs.CodeInvalidPath, "path traverses a symlink")
		}
	}
	return nil
}

// cleanPath is vfs.Clean plus the symlink-traversal check.
func (l *LocalFS) cleanPath(p string) (string, error) {
	c, err := vfs.Clean(p)
	if err != nil {
		return "", err
	}
	if err := l.noLinks(c); err != nil {
		return "", err
	}
	return c, nil
}

func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return vfs.E(vfs.CodeCanceled, "canceled")
	}
	return nil
}

func osName(clean string) string {
	if clean == "" {
		return "."
	}
	return clean
}

func baseName(clean string) string {
	if i := strings.LastIndexByte(clean, '/'); i >= 0 {
		return clean[i+1:]
	}
	return clean
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func entryOf(clean string, fi fs.FileInfo) vfs.Entry {
	e := vfs.Entry{
		Name:    baseName(clean),
		Path:    clean,
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
		Mode:    uint32(fi.Mode().Perm()),
	}
	switch m := fi.Mode(); {
	case m.IsRegular():
		e.Type = vfs.TypeFile
		e.ETag = vfs.ETag(fi.Size(), fi.ModTime())
	case m.IsDir():
		e.Type = vfs.TypeDir
		e.Size = 0
	case m&fs.ModeSymlink != 0:
		e.Type = vfs.TypeSymlink
		e.Size = 0
	default:
		e.Type = vfs.TypeOther
		e.Size = 0
	}
	return e
}

func (l *LocalFS) lstat(clean string) (fs.FileInfo, error) {
	fi, err := l.root.Lstat(osName(clean))
	return fi, mapErr(err)
}

func (l *LocalFS) List(ctx context.Context, p string) ([]vfs.Entry, error) {
	c, err := l.cleanPath(p)
	if err != nil {
		return nil, err
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	fi, err := l.lstat(c)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, vfs.E(vfs.CodeInvalidRequest, "not a directory")
	}
	d, err := l.root.Open(osName(c))
	if err != nil {
		return nil, mapErr(err)
	}
	defer d.Close()
	des, err := d.ReadDir(-1)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]vfs.Entry, 0, len(des))
	for _, de := range des {
		if c == "" && strings.EqualFold(de.Name(), vfs.PartDir) {
			continue
		}
		info, err := de.Info() // lstat semantics: never follows links
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // vanished while listing
			}
			return nil, mapErr(err)
		}
		out = append(out, entryOf(join(c, de.Name()), info))
	}
	return out, nil
}

func (l *LocalFS) Stat(ctx context.Context, p string) (vfs.Entry, error) {
	c, err := l.cleanPath(p)
	if err != nil {
		return vfs.Entry{}, err
	}
	if err := ctxErr(ctx); err != nil {
		return vfs.Entry{}, err
	}
	fi, err := l.lstat(c)
	if err != nil {
		return vfs.Entry{}, err
	}
	return entryOf(c, fi), nil
}

// openRegular opens a regular file, refusing symlinks and special files, and
// verifies the opened file is the one that was checked.
func (l *LocalFS) openRegular(c string) (*os.File, vfs.Entry, error) {
	if c == "" {
		return nil, vfs.Entry{}, vfs.E(vfs.CodeInvalidRequest, "not a regular file")
	}
	fi, err := l.lstat(c)
	if err != nil {
		return nil, vfs.Entry{}, err
	}
	if !fi.Mode().IsRegular() {
		return nil, vfs.Entry{}, vfs.E(vfs.CodeInvalidRequest, "not a regular file")
	}
	f, err := l.root.Open(c)
	if err != nil {
		return nil, vfs.Entry{}, mapErr(err)
	}
	fi2, err := f.Stat()
	if err != nil || !fi2.Mode().IsRegular() || !os.SameFile(fi, fi2) {
		f.Close()
		return nil, vfs.Entry{}, vfs.E(vfs.CodeChanged, "file changed while opening")
	}
	return f, entryOf(c, fi2), nil
}

func (l *LocalFS) Open(ctx context.Context, p string, offset int64, etag string) (io.ReadCloser, vfs.Entry, error) {
	c, err := l.cleanPath(p)
	if err != nil {
		return nil, vfs.Entry{}, err
	}
	if err := ctxErr(ctx); err != nil {
		return nil, vfs.Entry{}, err
	}
	if offset < 0 {
		return nil, vfs.Entry{}, vfs.E(vfs.CodeInvalidRequest, "negative offset")
	}
	f, e, err := l.openRegular(c)
	if err != nil {
		return nil, vfs.Entry{}, err
	}
	if etag != "" && etag != e.ETag {
		f.Close()
		return nil, vfs.Entry{}, vfs.E(vfs.CodeChanged, "etag mismatch")
	}
	if offset > e.Size {
		f.Close()
		return nil, vfs.Entry{}, vfs.E(vfs.CodeInvalidRequest, "offset beyond end of file")
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, vfs.Entry{}, mapErr(err)
		}
	}
	return f, e, nil
}

// ctxReader aborts on cancellation and remembers the first non-EOF error
// from the underlying reader, so source errors are not mistaken for local
// write errors.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
	err *error
}

func (c ctxReader) Read(b []byte) (int, error) {
	if err := ctxErr(c.ctx); err != nil {
		*c.err = err
		return 0, err
	}
	n, err := c.r.Read(b)
	if err != nil && err != io.EOF && *c.err == nil {
		*c.err = err
	}
	return n, err
}

// srcErr converts a reader-side error: vfs errors pass through, anything
// else is reported as internal with its text.
func srcErr(err error) error {
	var ve *vfs.Error
	if errors.As(err, &ve) {
		return ve
	}
	return vfs.E(vfs.CodeInternal, "source read: %v", err)
}

func (l *LocalFS) Hash(ctx context.Context, p string) (string, error) {
	c, err := l.cleanPath(p)
	if err != nil {
		return "", err
	}
	f, _, err := l.openRegular(c)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	var rerr error
	if _, err := io.Copy(h, ctxReader{ctx, f, &rerr}); err != nil {
		if rerr != nil {
			return "", srcErr(rerr)
		}
		return "", mapErr(err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (l *LocalFS) Put(ctx context.Context, p string, r io.Reader, o vfs.PutOptions) (vfs.Entry, error) {
	if err := l.writable(); err != nil {
		return vfs.Entry{}, err
	}
	c, err := l.cleanPath(p)
	if err != nil {
		return vfs.Entry{}, err
	}
	if c == "" {
		return vfs.Entry{}, vfs.E(vfs.CodeInvalidPath, "cannot write the share root")
	}
	if err := ctxErr(ctx); err != nil {
		return vfs.Entry{}, err
	}
	if !o.Overwrite {
		if _, err := l.root.Lstat(c); err == nil {
			return vfs.Entry{}, vfs.E(vfs.CodeExists, "already exists")
		}
	}
	if err := l.ensurePartDir(); err != nil {
		return vfs.Entry{}, err
	}
	var rnd [12]byte
	rand.Read(rnd[:])
	tmp := vfs.PartDir + "/put-" + hex.EncodeToString(rnd[:]) + tmpExt
	f, err := l.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return vfs.Entry{}, mapErr(err)
	}
	committed := false
	defer func() {
		if !committed {
			l.root.Remove(tmp)
		}
	}()
	h := sha256.New()
	var rerr error
	_, err = io.Copy(io.MultiWriter(f, h), ctxReader{ctx, r, &rerr})
	if err != nil && rerr != nil {
		f.Close()
		return vfs.Entry{}, srcErr(rerr)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return vfs.Entry{}, mapErr(err)
	}
	if o.SHA256 != "" && !strings.EqualFold(o.SHA256, hex.EncodeToString(h.Sum(nil))) {
		return vfs.Entry{}, vfs.E(vfs.CodeHashMismatch, "sha256 mismatch")
	}
	if !o.ModTime.IsZero() {
		if err := l.root.Chtimes(tmp, o.ModTime, o.ModTime); err != nil {
			return vfs.Entry{}, mapErr(err)
		}
	}
	if o.Overwrite {
		if fi, err := l.root.Lstat(c); err == nil && fi.IsDir() {
			return vfs.Entry{}, vfs.E(vfs.CodeExists, "a directory is in the way")
		}
		if err := l.root.Rename(tmp, c); err != nil {
			return vfs.Entry{}, mapErr(err)
		}
	} else if err := l.root.Link(tmp, c); err == nil {
		// Hard link fails if c exists, so this cannot replace a racing file.
		l.root.Remove(tmp)
	} else if errors.Is(err, fs.ErrExist) {
		return vfs.Entry{}, vfs.E(vfs.CodeExists, "already exists")
	} else {
		// Filesystem without hard links: check then rename.
		if _, serr := l.root.Lstat(c); serr == nil {
			return vfs.Entry{}, vfs.E(vfs.CodeExists, "already exists")
		}
		if err := l.root.Rename(tmp, c); err != nil {
			return vfs.Entry{}, mapErr(err)
		}
	}
	committed = true
	// On failure the new file stays at p, complete but not known to be durable.
	if err := l.syncParent(c); err != nil {
		return vfs.Entry{}, err
	}
	fi, err := l.lstat(c)
	if err != nil {
		return vfs.Entry{}, err
	}
	return entryOf(c, fi), nil
}

func (l *LocalFS) Mkdir(ctx context.Context, p string) error {
	if err := l.writable(); err != nil {
		return err
	}
	c, err := l.cleanPath(p)
	if err != nil {
		return err
	}
	if c == "" {
		return vfs.E(vfs.CodeExists, "share root exists")
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := l.root.Mkdir(c, 0o777); err != nil {
		return mapErr(err)
	}
	return l.syncParent(c)
}

func (l *LocalFS) Remove(ctx context.Context, p string, recursive bool) error {
	if err := l.writable(); err != nil {
		return err
	}
	c, err := l.cleanPath(p)
	if err != nil {
		return err
	}
	if c == "" {
		return vfs.E(vfs.CodeInvalidPath, "cannot remove the share root")
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	fi, err := l.lstat(c)
	if err != nil {
		return err
	}
	if fi.IsDir() && recursive {
		return mapErr(l.root.RemoveAll(c)) // never follows symlinks
	}
	err = l.root.Remove(c)
	if err != nil && fi.IsDir() && !isDiskFull(err) {
		// Windows reports a non-empty directory with its own errno.
		if d, oerr := l.root.Open(c); oerr == nil {
			names, _ := d.Readdirnames(1)
			d.Close()
			if len(names) > 0 {
				return vfs.E(vfs.CodeNotEmpty, "directory not empty")
			}
		}
	}
	return mapErr(err)
}

func (l *LocalFS) Rename(ctx context.Context, from, to string) error {
	if err := l.writable(); err != nil {
		return err
	}
	f, err := l.cleanPath(from)
	if err != nil {
		return err
	}
	t, err := l.cleanPath(to)
	if err != nil {
		return err
	}
	if f == "" || t == "" {
		return vfs.E(vfs.CodeInvalidPath, "cannot rename the share root")
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if t == f || strings.HasPrefix(t, f+"/") {
		return vfs.E(vfs.CodeInvalidRequest, "cannot move into itself")
	}
	src, err := l.lstat(f)
	if err != nil {
		return err
	}
	if dst, err := l.root.Lstat(t); err == nil {
		// Case-only rename on a case-insensitive FS resolves to the source itself.
		if !os.SameFile(src, dst) {
			return vfs.E(vfs.CodeExists, "destination exists")
		}
	}
	return mapErr(l.root.Rename(f, t))
}
