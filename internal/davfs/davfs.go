// Package davfs serves the mesh as a loopback WebDAV tree:
// /{peer}/{share}/{path}, where / and /{peer} are read-only virtual
// directories.
package davfs

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/vfs"
)

// User is the Basic auth user name; the password is the control token.
const User = "sugar-it"

const preflightTimeout = 5 * time.Second

// Router is the part of the node WebDAV needs.
type Router interface {
	PeerNames() []string // includes "local"
	Shares(ctx context.Context, peer string) ([]peerapi.ShareInfo, error)
	FS(ctx context.Context, peer, share string) (vfs.FS, error)
}

// Handler serves the mesh behind HTTP Basic auth. Requests must name a
// loopback host (a browser reaching the port through DNS rebinding sends its
// own host name) and carry user "sugar-it" with the token as password.
// cacheTTL bounds how long listings and share lists are reused; zero
// disables caching.
func Handler(r Router, token string, cacheTTL time.Duration) http.Handler {
	fsys := &fileSystem{r: r, c: newCache(cacheTTL), started: time.Now()}
	return &handler{
		fs:      fsys,
		dav:     &webdav.Handler{FileSystem: fsys, LockSystem: webdav.NewMemLS()},
		hasPass: token != "",
		user:    sha256.Sum256([]byte(User)),
		pass:    sha256.Sum256([]byte(token)),
	}
}

type handler struct {
	fs         *fileSystem
	dav        *webdav.Handler
	hasPass    bool
	user, pass [sha256.Size]byte
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopbackHost(r.Host) {
		http.Error(w, "the WebDAV server only answers to a loopback host name", http.StatusForbidden)
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="sugar-it", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !h.preflight(w, r) {
		return
	}
	if r.Method == http.MethodPut {
		st := &bodyState{want: r.ContentLength}
		r.Body = &trackBody{ReadCloser: r.Body, st: st}
		r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, st))
	}
	h.dav.ServeHTTP(w, r)
}

func (h *handler) authorized(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	us, ps := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
	// Hashing first makes the comparison independent of the lengths.
	okU := subtle.ConstantTimeCompare(us[:], h.user[:])
	okP := subtle.ConstantTimeCompare(ps[:], h.pass[:])
	return ok && h.hasPass && okU&okP == 1
}

// loopbackHost mirrors the Control API's rule.
func loopbackHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	} else {
		h = strings.Trim(h, "[]")
	}
	if h == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.IsLoopback()
}

// preflight answers what x/net's handler cannot express: it maps most
// filesystem errors to 404 or 405. It rejects writes to virtual directories
// and read-only shares (403), cross-share MOVE (502) and unreachable peers
// (5xx, within a few seconds). It returns false once it has replied.
func (h *handler) preflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), preflightTimeout)
	defer cancel()
	src := parse(r.URL.Path)
	fail := func(status int, msg string) bool {
		http.Error(w, msg, status)
		return false
	}
	check := func(l loc, write bool) bool {
		if write && l.depth < 3 {
			return fail(http.StatusForbidden, "peers and shares are read-only directories")
		}
		switch {
		case l.depth == 1:
			if _, err := h.fs.shares(ctx, l.peer); err != nil {
				return fail(statusOfErr(err), errText(err))
			}
		case l.depth >= 2:
			_, ro, err := h.fs.target(ctx, l)
			if err != nil {
				return fail(statusOfErr(err), errText(err))
			}
			if write && ro {
				return fail(http.StatusForbidden, "the share is read-only")
			}
		}
		return true
	}
	switch r.Method {
	case "PUT", "MKCOL", "DELETE", "PROPPATCH", "LOCK":
		return check(src, true)
	case "GET", "HEAD":
		// A download needs the current size and ETag, not the cached ones.
		h.fs.dropList(src)
		return check(src, false)
	case "MOVE", "COPY":
		move := r.Method == "MOVE"
		if !check(src, move) {
			return false
		}
		u, err := url.Parse(r.Header.Get("Destination"))
		if err != nil || r.Header.Get("Destination") == "" || (u.Host != "" && u.Host != r.Host) {
			return true // x/net reports the malformed destination
		}
		dst := parse(u.Path)
		if move && src.depth >= 2 && dst.depth >= 2 && (src.peer != dst.peer || src.share != dst.share) {
			return fail(http.StatusBadGateway, vfs.CodeCrossHostRename+": moves across hosts or shares are done in the app")
		}
		return check(dst, true)
	}
	return check(src, false)
}

func statusOfErr(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	switch vfs.CodeOf(err) {
	case vfs.CodePeerOffline, vfs.CodePeerIncompatible:
		return http.StatusBadGateway
	case vfs.CodePeerNotFound, vfs.CodeShareNotFound, vfs.CodeNotFound:
		return http.StatusNotFound
	case vfs.CodePeerUnauthorized, vfs.CodeReadOnly:
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func errText(err error) string {
	var e *vfs.Error
	if errors.As(err, &e) {
		return e.Error()
	}
	return "unavailable"
}

// loc is a WebDAV path split into the mesh tree.
type loc struct {
	peer, share, rest string
	depth             int // 0 root, 1 peer, 2 share root, 3 inside a share
}

func parse(name string) loc {
	p := strings.Trim(path.Clean("/"+name), "/")
	if p == "" {
		return loc{}
	}
	parts := strings.SplitN(p, "/", 3)
	l := loc{peer: parts[0], depth: 1}
	if len(parts) > 1 {
		l.share, l.depth = parts[1], 2
	}
	if len(parts) > 2 {
		l.rest, l.depth = parts[2], 3
	}
	return l
}

func (l loc) parent() loc {
	if l.depth < 3 {
		return loc{peer: l.peer, share: l.share, depth: max(l.depth-1, 0)}
	}
	p := loc{peer: l.peer, share: l.share, depth: 2}
	if i := strings.LastIndexByte(l.rest, '/'); i >= 0 {
		p.rest, p.depth = l.rest[:i], 3
	}
	return p
}

func (l loc) base() string {
	switch l.depth {
	case 0:
		return "/"
	case 1:
		return l.peer
	case 2:
		return l.share
	}
	return path.Base(l.rest)
}

func listKey(l loc) string { return "l\x00" + l.peer + "\x00" + l.share + "\x00" + l.rest }

// fileSystem is the webdav.FileSystem over the mesh.
type fileSystem struct {
	r       Router
	c       *cache
	started time.Time
}

var _ webdav.FileSystem = (*fileSystem)(nil)

func pathErr(op, name string, err error) error {
	if err == nil {
		return nil
	}
	var base error
	switch vfs.CodeOf(err) {
	case vfs.CodeNotFound, vfs.CodeShareNotFound, vfs.CodePeerNotFound:
		base = os.ErrNotExist
	case vfs.CodeExists:
		base = os.ErrExist
	case vfs.CodeReadOnly, vfs.CodePeerUnauthorized:
		base = os.ErrPermission
	default:
		return &os.PathError{Op: op, Path: name, Err: err}
	}
	return &os.PathError{Op: op, Path: name, Err: base}
}

func (f *fileSystem) shares(ctx context.Context, peer string) ([]peerapi.ShareInfo, error) {
	k := "s\x00" + peer
	if v, ok := f.c.get(k); ok {
		return v.([]peerapi.ShareInfo), nil
	}
	gen := f.c.generation()
	s, err := f.r.Shares(ctx, peer)
	if err != nil {
		return nil, err
	}
	f.c.put(k, s, gen)
	return s, nil
}

// target resolves the share of l and whether it is read-only.
func (f *fileSystem) target(ctx context.Context, l loc) (vfs.FS, bool, error) {
	infos, err := f.shares(ctx, l.peer)
	if err != nil {
		return nil, false, err
	}
	i := slices.IndexFunc(infos, func(s peerapi.ShareInfo) bool { return s.Name == l.share })
	if i < 0 {
		return nil, false, vfs.E(vfs.CodeShareNotFound, "no share %q on %s", l.share, l.peer)
	}
	k := "f\x00" + l.peer + "\x00" + l.share
	if v, ok := f.c.get(k); ok {
		return v.(vfs.FS), infos[i].ReadOnly, nil
	}
	gen := f.c.generation()
	fsys, err := f.r.FS(ctx, l.peer, l.share)
	if err != nil {
		return nil, false, err
	}
	f.c.put(k, fsys, gen)
	return fsys, infos[i].ReadOnly, nil
}

// list returns the files and directories in the directory l (depth >= 2).
// Symlinks and special files are not shown: they are never followed.
func (f *fileSystem) list(ctx context.Context, l loc) ([]vfs.Entry, error) {
	k := listKey(l)
	if v, ok := f.c.get(k); ok {
		return v.([]vfs.Entry), nil
	}
	gen := f.c.generation()
	fsys, _, err := f.target(ctx, l)
	if err != nil {
		return nil, err
	}
	all, err := fsys.List(ctx, l.rest)
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, e := range all {
		if e.Type == vfs.TypeFile || e.Type == vfs.TypeDir {
			out = append(out, e)
		}
	}
	f.c.put(k, out, gen)
	return out, nil
}

func (f *fileSystem) virtualDir(name string) info {
	return info{name: name, dir: true, mt: f.started}
}

// stat answers from the parent's (cached) listing, so a PROPFIND over a big
// directory costs one List and no Stat.
func (f *fileSystem) stat(ctx context.Context, l loc) (info, error) {
	switch l.depth {
	case 0:
		return f.virtualDir("/"), nil
	case 1:
		if !slices.Contains(f.r.PeerNames(), l.peer) {
			return info{}, os.ErrNotExist
		}
		return f.virtualDir(l.peer), nil
	case 2:
		infos, err := f.shares(ctx, l.peer)
		if err != nil {
			return info{}, err
		}
		if !slices.ContainsFunc(infos, func(s peerapi.ShareInfo) bool { return s.Name == l.share }) {
			return info{}, os.ErrNotExist
		}
		return f.virtualDir(l.share), nil
	}
	ents, err := f.list(ctx, l.parent())
	if err != nil {
		if c := vfs.CodeOf(err); c == vfs.CodeInvalidRequest || c == vfs.CodeInvalidPath {
			return info{}, os.ErrNotExist // a file in the path, or an unusable name
		}
		return info{}, err
	}
	name := l.base()
	for _, e := range ents {
		if e.Name == name {
			return infoOf(e), nil
		}
	}
	return info{}, os.ErrNotExist
}

func (f *fileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	i, err := f.stat(ctx, parse(name))
	if err != nil {
		return nil, pathErr("stat", name, err)
	}
	return i, nil
}

func (f *fileSystem) children(ctx context.Context, l loc) ([]os.FileInfo, error) {
	switch l.depth {
	case 0:
		var out []os.FileInfo
		for _, p := range f.r.PeerNames() {
			out = append(out, f.virtualDir(p))
		}
		return out, nil
	case 1:
		infos, err := f.shares(ctx, l.peer)
		if err != nil {
			return nil, err
		}
		var out []os.FileInfo
		for _, s := range infos {
			out = append(out, f.virtualDir(s.Name))
		}
		return out, nil
	}
	ents, err := f.list(ctx, l)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, len(ents))
	for i, e := range ents {
		out[i] = infoOf(e)
	}
	return out, nil
}

func (f *fileSystem) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	l := parse(name)
	if flag&(os.O_CREATE|os.O_TRUNC) != 0 {
		return f.openWrite(ctx, name, l, flag)
	}
	self, err := f.stat(ctx, l)
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	if self.dir {
		return &dirFile{self: self, load: func() ([]os.FileInfo, error) {
			c, err := f.children(ctx, l)
			return c, pathErr("readdir", name, err)
		}}, nil
	}
	fsys, _, err := f.target(ctx, l)
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	return &readFile{ctx: ctx, fs: fsys, path: l.rest, self: self}, nil
}

func (f *fileSystem) openWrite(ctx context.Context, name string, l loc, flag int) (webdav.File, error) {
	if l.depth < 3 {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
	}
	fsys, ro, err := f.target(ctx, l)
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	if ro {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
	}
	if p := l.parent(); p.depth >= 3 {
		pi, err := f.stat(ctx, p)
		if err != nil {
			return nil, pathErr("open", name, err)
		}
		if !pi.dir {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
		}
	}
	if cur, err := f.stat(ctx, l); err == nil {
		if cur.dir {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrExist}
		}
		if flag&os.O_EXCL != 0 {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrExist}
		}
	}
	sp, err := os.CreateTemp("", "sugar-it-dav-*") // 0600, in the OS temp dir
	if err != nil {
		return nil, err
	}
	return &writeFile{fsys: f, ctx: ctx, l: l, target: fsys, name: name, overwrite: flag&os.O_EXCL == 0, spool: sp}, nil
}

func (f *fileSystem) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	l := parse(name)
	if l.depth < 3 {
		return &os.PathError{Op: "mkdir", Path: name, Err: os.ErrPermission}
	}
	fsys, _, err := f.target(ctx, l)
	if err != nil {
		return pathErr("mkdir", name, err)
	}
	defer f.changed(l)
	return pathErr("mkdir", name, fsys.Mkdir(ctx, l.rest))
}

func (f *fileSystem) RemoveAll(ctx context.Context, name string) error {
	l := parse(name)
	if l.depth < 3 {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrPermission}
	}
	fsys, _, err := f.target(ctx, l)
	if err != nil {
		return pathErr("remove", name, err)
	}
	defer f.changed(l)
	if err := fsys.Remove(ctx, l.rest, true); err != nil && vfs.CodeOf(err) != vfs.CodeNotFound {
		return pathErr("remove", name, err)
	}
	return nil
}

func (f *fileSystem) Rename(ctx context.Context, oldName, newName string) error {
	from, to := parse(oldName), parse(newName)
	if from.depth < 3 || to.depth < 3 {
		return &os.LinkError{Op: "rename", Old: oldName, New: newName, Err: os.ErrPermission}
	}
	if from.peer != to.peer || from.share != to.share {
		return &os.LinkError{Op: "rename", Old: oldName, New: newName,
			Err: vfs.E(vfs.CodeCrossHostRename, "moves across hosts or shares are done in the app")}
	}
	fsys, _, err := f.target(ctx, from)
	if err != nil {
		return pathErr("rename", oldName, err)
	}
	defer f.changed(from)
	defer f.changed(to)
	return pathErr("rename", oldName, fsys.Rename(ctx, from.rest, to.rest))
}

// changed drops what a write at l may have made stale: the parent's listing
// and, if l is a directory, everything below it.
func (f *fileSystem) changed(l loc) {
	if l.depth < 3 {
		return
	}
	f.c.drop([]string{listKey(l.parent()), listKey(l)}, []string{listKey(loc{peer: l.peer, share: l.share, rest: l.rest + "/"})})
}

// dropList forgets the listing that holds l, for requests that need it fresh.
func (f *fileSystem) dropList(l loc) {
	if l.depth >= 3 {
		f.c.drop([]string{listKey(l.parent())}, nil)
	}
}
