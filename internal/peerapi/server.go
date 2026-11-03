package peerapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"sugarintegration/internal/identity"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

type callerKey struct{}

type server struct {
	b    Backend
	goos string // whose file name rules destination paths must satisfy
}

// NewServer returns the Peer API server. Addr is left to the caller, who
// serves it with ServeTLS(ln, "", ""): the certificate is in TLSConfig.
func NewServer(id *identity.Identity, b Backend) *http.Server {
	return newServer(id, b, runtime.GOOS)
}

func newServer(id *identity.Identity, b Backend, goos string) *http.Server {
	s := &server{b: b, goos: goos}
	return &http.Server{
		Handler: s.handler(),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			ClientAuth:   tls.RequireAnyClientCert, // identity is checked per request
			Certificates: []tls.Certificate{id.Cert},
		},
		ErrorLog:          log.New(&filterWriter{w: os.Stderr}, "peerapi: ", log.LstdFlags),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No Read/WriteTimeout: file transfers are arbitrarily long.
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.info)
	mux.HandleFunc("POST /v1/pair", s.pair)
	mux.HandleFunc("POST /v1/members/sync", s.sync)
	mux.HandleFunc("GET /v1/shares", s.shares)
	mux.HandleFunc("GET /v1/shares/{s}/entries", s.entries)
	mux.HandleFunc("GET /v1/shares/{s}/stat", s.stat)
	mux.HandleFunc("GET /v1/shares/{s}/file", s.download)
	mux.HandleFunc("PUT /v1/shares/{s}/file", s.upload)
	mux.HandleFunc("GET /v1/shares/{s}/hash", s.hash)
	mux.HandleFunc("POST /v1/shares/{s}/mkdir", s.mkdir)
	mux.HandleFunc("POST /v1/shares/{s}/remove", s.remove)
	mux.HandleFunc("POST /v1/shares/{s}/rename", s.rename)
	mux.HandleFunc("POST /v1/jobs", s.submitJob)
	mux.HandleFunc("GET /v1/jobs", s.listJobs)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("POST /v1/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, vfs.E(vfs.CodeNotFound, "no such endpoint"))
	})
	return s.authenticate(mux)
}

// authenticate runs before routing, so no endpoint (known or not) is reachable
// by a certificate that is not a member's, except pairing.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeErr(w, vfs.E(vfs.CodePeerUnauthorized, "client certificate required"))
			return
		}
		caller := identity.IDOf(r.TLS.PeerCertificates[0])
		ctx := context.WithValue(r.Context(), callerKey{}, caller)
		if r.URL.EscapedPath() != "/v1/pair" {
			if !s.b.IsMember(caller) {
				writeErr(w, vfs.E(vfs.CodePeerUnauthorized, "not a member"))
				return
			}
			// Asked only for members, so a stranger cannot grow the backend's
			// state. Done already if the member was removed just now.
			member := s.b.CallerContext(caller)
			if member.Err() != nil {
				writeErr(w, vfs.E(vfs.CodePeerUnauthorized, "not a member"))
				return
			}
			var cancel context.CancelCauseFunc
			ctx, cancel = context.WithCancelCause(ctx)
			defer cancel(nil)
			defer context.AfterFunc(member, func() { cancel(errRevoked) })()
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// errRevoked is the cause of a request's context when its caller was removed.
var errRevoked = errors.New("the caller was removed")

// revokedOr is err, or peer_unauthorized if err is due to the caller's removal.
func revokedOr(r *http.Request, err error) error {
	if errors.Is(context.Cause(r.Context()), errRevoked) {
		return vfs.E(vfs.CodePeerUnauthorized, "not a member")
	}
	return err
}

// onCancel runs f when the request's context ends, unless the returned stop
// was called first. Reads and writes blocked on the network look at no
// context; f gives them a deadline. The ResponseWriter is unusable once the
// handler has returned, so stop waits out an f that already started.
func onCancel(r *http.Request, f func()) (stop func()) {
	var mu sync.Mutex
	finished := false
	unregister := context.AfterFunc(r.Context(), func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			f()
		}
	})
	return func() {
		unregister()
		mu.Lock()
		finished = true
		mu.Unlock()
	}
}

// ctxReader fails once ctx is done, between reads of a source that does not
// look at ctx itself.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

func caller(r *http.Request) string { return r.Context().Value(callerKey{}).(string) }

// ---- response helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// wireErr is the error as sent to a peer: vfs errors keep their text; anything
// else is reported without detail, since it may name local paths.
func wireErr(err error) *vfs.Error {
	var ve *vfs.Error
	if errors.As(err, &ve) && knownCode(ve.Code) {
		return ve
	}
	return vfs.E(vfs.CodeInternal, "internal error")
}

func writeErr(w http.ResponseWriter, err error) {
	ve := wireErr(err)
	writeJSON(w, statusOf[ve.Code], ve)
}

// decode reads a size-limited JSON body.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// A stranger may reach /v1/pair, so a stalled body must not hold the
	// handler open. Only JSON bodies get a deadline; uploads do not.
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(jsonReadTimeout))
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeJSON(w, http.StatusRequestEntityTooLarge, vfs.E(vfs.CodeInvalidRequest, "request body too large"))
		} else {
			writeErr(w, vfs.E(vfs.CodeInvalidRequest, "malformed JSON body"))
		}
		return false
	}
	return true
}

// fsFor resolves the share named in the URL.
func (s *server) fsFor(w http.ResponseWriter, r *http.Request) (vfs.FS, bool) {
	f, err := s.b.ShareFS(r.PathValue("s"))
	if err != nil {
		writeErr(w, err)
		return nil, false
	}
	return f, true
}

// srcPath cleans the ?path= query value.
func srcPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, err := vfs.Clean(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	return p, true
}

// dstPath cleans a destination path and checks every component against this
// OS's file name rules.
func (s *server) dstPath(p string) (string, error) {
	c, err := vfs.Clean(p)
	if err != nil {
		return "", err
	}
	if c == "" {
		return c, nil
	}
	for _, comp := range strings.Split(c, "/") {
		if !shares.ValidName(s.goos, comp) {
			return "", vfs.E(vfs.CodeInvalidName, "%q is not a valid file name here", comp)
		}
	}
	return c, nil
}

// ---- endpoints ----

func (s *server) info(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.b.Info()) }

func (s *server) shares(w http.ResponseWriter, r *http.Request) {
	list := s.b.Shares()
	if list == nil {
		list = []ShareInfo{}
	}
	writeJSON(w, 200, list)
}

func (s *server) pair(w http.ResponseWriter, r *http.Request) {
	var req PairRequest
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.b.Pair(r.Context(), caller(r), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, resp)
}

func (s *server) sync(w http.ResponseWriter, r *http.Request) {
	var in []mesh.Member
	if !decode(w, r, &in) {
		return
	}
	out, err := s.b.SyncMembers(r.Context(), caller(r), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	if out == nil {
		out = []mesh.Member{}
	}
	writeJSON(w, 200, out)
}

func (s *server) entries(w http.ResponseWriter, r *http.Request) {
	f, ok := s.fsFor(w, r)
	if !ok {
		return
	}
	p, ok := srcPath(w, r)
	if !ok {
		return
	}
	es, err := f.List(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	if es == nil {
		es = []vfs.Entry{}
	}
	writeJSON(w, 200, es)
}

func (s *server) stat(w http.ResponseWriter, r *http.Request) {
	f, ok := s.fsFor(w, r)
	if !ok {
		return
	}
	p, ok := srcPath(w, r)
	if !ok {
		return
	}
	e, err := f.Stat(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, e)
}

// parseRange accepts only "bytes=N-": resuming from an offset is the one
// range this protocol uses.
func parseRange(h string) (int64, error) {
	if h == "" {
		return 0, nil
	}
	v, ok := strings.CutPrefix(h, "bytes=")
	v, ok2 := strings.CutSuffix(v, "-")
	off, err := strconv.ParseInt(v, 10, 64)
	if !ok || !ok2 || err != nil || off < 0 {
		return 0, vfs.E(vfs.CodeInvalidRequest, "unsupported Range")
	}
	return off, nil
}

// download serves a file from an offset. If-Range carries the etag the caller
// expects; unlike plain HTTP, a mismatch is an error (changed), not a full
// body, so a resume can never splice two versions of a file.
func (s *server) download(w http.ResponseWriter, r *http.Request) {
	f, ok := s.fsFor(w, r)
	if !ok {
		return
	}
	p, ok := srcPath(w, r)
	if !ok {
		return
	}
	off, err := parseRange(r.Header.Get("Range"))
	if err != nil {
		writeErr(w, err)
		return
	}
	rc, e, err := f.Open(r.Context(), p, off, r.Header.Get("If-Range"))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rc.Close()
	n := e.Size - off
	if n < 0 {
		writeErr(w, vfs.E(vfs.CodeInvalidRequest, "offset beyond end of file"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", `"`+e.ETag+`"`)
	h.Set("Last-Modified", e.ModTime.UTC().Format(http.TimeFormat))
	h.Set(entryHeader, encodeEntry(e))
	status := http.StatusOK
	if off > 0 {
		status = http.StatusPartialContent
		if n > 0 {
			h.Set("Content-Range", "bytes "+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(e.Size-1, 10)+"/"+strconv.FormatInt(e.Size, 10))
		}
	}
	w.WriteHeader(status)
	if n == 0 {
		return
	}
	// A client that stopped reading must not keep a removed member's stream.
	defer onCancel(r, func() { http.NewResponseController(w).SetWriteDeadline(time.Now()) })()
	// The promised length is a promise: if the file shrank or the copy fails,
	// abort the stream so the client sees an error rather than a clean EOF.
	if copied, err := io.Copy(w, io.LimitReader(ctxReader{r.Context(), rc}, n)); err != nil || copied != n {
		panic(http.ErrAbortHandler)
	}
}

func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	p, err := s.dstPath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var o vfs.PutOptions
	q := r.URL.Query()
	o.Overwrite = q.Get("overwrite") == "1"
	o.SHA256 = q.Get("sha256")
	if m := q.Get("mtime"); m != "" {
		ns, err := strconv.ParseInt(m, 10, 64)
		if err != nil {
			writeErr(w, vfs.E(vfs.CodeInvalidRequest, "bad mtime"))
			return
		}
		o.ModTime = time.Unix(0, ns)
	}
	f, ok := s.fsFor(w, r)
	if !ok {
		return
	}
	// A stalled uploader must not keep a removed member's request open.
	stop := onCancel(r, func() { http.NewResponseController(w).SetReadDeadline(time.Now()) })
	e, err := f.Put(r.Context(), p, r.Body, o) // the body is the file: not size-limited
	stop()
	if err != nil {
		writeErr(w, revokedOr(r, err))
		return
	}
	writeJSON(w, 200, e)
}

// hash flushes the headers before hashing so a long hash cannot trip the
// client's response-header timeout. The outcome is therefore the body:
// {"sha256"} or {"code","message"}.
func (s *server) hash(w http.ResponseWriter, r *http.Request) {
	f, ok := s.fsFor(w, r)
	if !ok {
		return
	}
	p, ok := srcPath(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush()
	sum, err := f.Hash(r.Context(), p)
	if err != nil {
		ve := wireErr(revokedOr(r, err))
		json.NewEncoder(w).Encode(hashBody{Code: ve.Code, Message: ve.Message})
		return
	}
	json.NewEncoder(w).Encode(hashBody{SHA256: sum})
}

func (s *server) mkdir(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if !decode(w, r, &req) {
		return
	}
	p, err := s.dstPath(req.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	f, ok := s.fsFor(w, r)
	if !ok {
		return
	}
	s.done(w, f.Mkdir(r.Context(), p))
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	var req removeReq
	if !decode(w, r, &req) {
		return
	}
	p, err := vfs.Clean(req.Path) // the source of a delete must be nameable as it is
	if err != nil {
		writeErr(w, err)
		return
	}
	f, ok := s.fsFor(w, r)
	if !ok {
		return
	}
	s.done(w, f.Remove(r.Context(), p, req.Recursive))
}

func (s *server) rename(w http.ResponseWriter, r *http.Request) {
	var req renameReq
	if !decode(w, r, &req) {
		return
	}
	from, err := vfs.Clean(req.From)
	if err != nil {
		writeErr(w, err)
		return
	}
	to, err := s.dstPath(req.To)
	if err != nil {
		writeErr(w, err)
		return
	}
	f, ok := s.fsFor(w, r)
	if !ok {
		return
	}
	s.done(w, f.Rename(r.Context(), from, to))
}

func (s *server) done(w http.ResponseWriter, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) submitJob(w http.ResponseWriter, r *http.Request) {
	var spec transfer.Spec
	if !decode(w, r, &spec) {
		return
	}
	j, err := s.b.SubmitJob(caller(r), spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, j)
}

func (s *server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.b.ListJobs(r.URL.Query().Get("origin"))
	if jobs == nil {
		jobs = []transfer.Job{}
	}
	writeJSON(w, 200, jobs)
}

func (s *server) cancelJob(w http.ResponseWriter, r *http.Request) {
	s.done(w, s.b.CancelJob(r.PathValue("id")))
}

func (s *server) retryJob(w http.ResponseWriter, r *http.Request) {
	s.done(w, s.b.RetryJob(r.PathValue("id")))
}

// jsonReadTimeout bounds how long a JSON request body may take to arrive.
var jsonReadTimeout = 10 * time.Second

type filterWriter struct{ w io.Writer }

// Write drops TLS handshake failures, which every port scan and pinned-ID
// mismatch produces, and passes everything else (notably handler panics).
func (f *filterWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("TLS handshake error")) {
		return len(p), nil
	}
	return f.w.Write(p)
}
