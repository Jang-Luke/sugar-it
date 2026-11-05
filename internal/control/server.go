package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"sugarintegration/internal/node"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

const (
	maxBody         = 1 << 20
	peerJobsTimeout = 5 * time.Second
)

// Deps is what the Control API serves.
type Deps struct {
	Node      *node.Node
	Jobs      *transfer.Manager
	Token     string
	InProcess bool
	// Shutdown asks the host to stop; it returns at once. Nil: not supported.
	Shutdown func()
}

type server struct{ Deps }

// NewServer returns the Control API server. Addr is left to the caller, who
// serves it on a loopback listener. Every request must name a loopback host
// (so DNS rebinding cannot reach it) and carry the bearer token. CORS is not
// enabled: no browser page is a legitimate caller.
func NewServer(d Deps) *http.Server {
	s := &server{d}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("POST /v1/shutdown", s.shutdown)
	mux.HandleFunc("POST /v1/invite", s.invite)
	mux.HandleFunc("POST /v1/peers/join", s.join)
	mux.HandleFunc("POST /v1/peers/remove", s.removePeer)
	mux.HandleFunc("POST /v1/self/rename", s.renameSelf)
	mux.HandleFunc("GET /v1/shares", s.listShares)
	mux.HandleFunc("POST /v1/shares/add", s.addShare)
	mux.HandleFunc("POST /v1/shares/remove", s.removeShare)
	mux.HandleFunc("POST /v1/fs/list", s.list)
	mux.HandleFunc("POST /v1/fs/mkdir", s.mkdir)
	mux.HandleFunc("POST /v1/fs/remove", s.remove)
	mux.HandleFunc("POST /v1/fs/rename", s.rename)
	mux.HandleFunc("POST /v1/jobs", s.startJob)
	mux.HandleFunc("GET /v1/jobs", s.jobs)
	mux.HandleFunc("GET /v1/jobs/{host}/{id}", s.job)
	mux.HandleFunc("POST /v1/jobs/{host}/{id}/cancel", s.cancelJob)
	mux.HandleFunc("POST /v1/jobs/{host}/{id}/retry", s.retryJob)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, vfs.E(vfs.CodeNotFound, "no such endpoint"))
	})
	return &http.Server{
		Handler:           s.guard(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: a request may wait on a slow peer.
	}
}

// guard checks the Host header, then the bearer token.
func (s *server) guard(next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(s.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeStatus(w, http.StatusForbidden, vfs.E(vfs.CodePeerUnauthorized, "the control API only answers to a loopback host name"))
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(tok))
		// Hashing first makes the comparison independent of the token's length.
		if !ok || s.Token == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeStatus(w, http.StatusUnauthorized, vfs.E(vfs.CodePeerUnauthorized, "missing or invalid control token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

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

// ---- responses ----

var statusOf = map[string]int{
	vfs.CodeNotFound:         http.StatusNotFound,
	vfs.CodeExists:           http.StatusConflict,
	vfs.CodeNotEmpty:         http.StatusConflict,
	vfs.CodeReadOnly:         http.StatusForbidden,
	vfs.CodeInvalidPath:      http.StatusBadRequest,
	vfs.CodeInvalidName:      http.StatusBadRequest,
	vfs.CodeInvalidRequest:   http.StatusBadRequest,
	vfs.CodeShareNotFound:    http.StatusNotFound,
	vfs.CodeShareOverlap:     http.StatusConflict,
	vfs.CodePeerNotFound:     http.StatusNotFound,
	vfs.CodePeerOffline:      http.StatusBadGateway,
	vfs.CodePeerUnauthorized: http.StatusForbidden,
	vfs.CodePeerIncompatible: http.StatusConflict,
	vfs.CodeChanged:          http.StatusPreconditionFailed,
	vfs.CodeDiskFull:         http.StatusInsufficientStorage,
	vfs.CodeHashMismatch:     http.StatusUnprocessableEntity,
	vfs.CodeConflict:         http.StatusConflict,
	vfs.CodeCrossHostRename:  http.StatusBadRequest,
	vfs.CodeCanceled:         http.StatusRequestTimeout,
	vfs.CodeInternal:         http.StatusInternalServerError,
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, status int, e *vfs.Error) { writeJSON(w, status, e) }

func writeErr(w http.ResponseWriter, err error) {
	var ve *vfs.Error
	if !errors.As(err, &ve) {
		ve = vfs.E(vfs.CodeInternal, "%v", err)
	}
	st, ok := statusOf[ve.Code]
	if !ok {
		st = http.StatusInternalServerError
	}
	writeJSON(w, st, ve)
}

func done(w http.ResponseWriter, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, vfs.E(vfs.CodeInvalidRequest, "bad request body: %v", err))
		return false
	}
	return true
}

// ---- mesh ----

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	peers := s.Node.Peers()
	if peers == nil {
		peers = []node.PeerStatus{}
	}
	writeJSON(w, 200, Status{ID: s.Node.ID(), Name: s.Node.Name(), Peers: peers, Version: Version, InProcess: s.InProcess})
}

// shutdown replies first, then stops the daemon: the reply must not wait for
// the server that sends it. A daemon hosted by the desktop app is the app's.
func (s *server) shutdown(w http.ResponseWriter, r *http.Request) {
	if s.InProcess || s.Deps.Shutdown == nil {
		writeErr(w, vfs.E(vfs.CodeInvalidRequest, "this daemon runs inside the sugar-it desktop app; quit the app to stop it"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
	s.Deps.Shutdown()
}

func (s *server) invite(w http.ResponseWriter, r *http.Request) {
	var req inviteReq
	if !decode(w, r, &req) {
		return
	}
	inv, err := s.Node.Invite(time.Duration(req.TTLSeconds) * time.Second)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, inviteResp{inv})
}

func (s *server) join(w http.ResponseWriter, r *http.Request) {
	var req joinReq
	if !decode(w, r, &req) {
		return
	}
	p, err := s.Node.Join(r.Context(), req.Invite)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *server) removePeer(w http.ResponseWriter, r *http.Request) {
	var req nameReq
	if !decode(w, r, &req) {
		return
	}
	done(w, s.Node.RemovePeer(req.Name))
}

func (s *server) renameSelf(w http.ResponseWriter, r *http.Request) {
	var req nameReq
	if !decode(w, r, &req) {
		return
	}
	done(w, s.Node.RenameSelf(req.Name))
}

// ---- shares and files ----

func (s *server) listShares(w http.ResponseWriter, r *http.Request) {
	peer := r.URL.Query().Get("peer")
	if peer == "" {
		peer = "local"
	}
	infos, err := s.Node.Shares(r.Context(), peer)
	if err != nil {
		writeErr(w, err)
		return
	}
	if infos == nil {
		infos = []peerapi.ShareInfo{}
	}
	writeJSON(w, 200, infos)
}

func (s *server) addShare(w http.ResponseWriter, r *http.Request) {
	var sp shares.Spec
	if !decode(w, r, &sp) {
		return
	}
	done(w, s.Node.AddShare(sp))
}

func (s *server) removeShare(w http.ResponseWriter, r *http.Request) {
	var req nameReq
	if !decode(w, r, &req) {
		return
	}
	done(w, s.Node.RemoveShare(req.Name))
}

// fsFor opens the share a target names and returns its cleaned path.
func (s *server) fsFor(ctx context.Context, t Target) (fs vfs.FS, clean string, local bool, err error) {
	if t.Share == "" {
		return nil, "", false, vfs.E(vfs.CodeInvalidRequest, "a share is required (peer:share/path)")
	}
	if clean, err = vfs.Clean(t.Path); err != nil {
		return nil, "", false, err
	}
	_, local, err = s.Node.ResolvePeer(t.Peer)
	if err != nil {
		return nil, "", false, err
	}
	fs, err = s.Node.FS(ctx, t.Peer, t.Share)
	return fs, clean, local, err
}

// validName checks a name about to be created. A remote peer applies its own
// OS's rules as well; here only the baseline (and this OS's, for local files).
func validName(local bool, name string) error {
	goos := "linux"
	if local {
		goos = runtime.GOOS
	}
	if !shares.ValidName(goos, name) {
		return vfs.E(vfs.CodeInvalidName, "%q is not a valid file name", name)
	}
	return nil
}

func validPath(local bool, p string) error {
	if p == "" {
		return nil
	}
	for _, c := range strings.Split(p, "/") {
		if err := validName(local, c); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	var req targetReq
	if !decode(w, r, &req) {
		return
	}
	fs, p, _, err := s.fsFor(r.Context(), req.Target)
	if err != nil {
		writeErr(w, err)
		return
	}
	es, err := fs.List(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	if es == nil {
		es = []vfs.Entry{}
	}
	writeJSON(w, 200, es)
}

func (s *server) mkdir(w http.ResponseWriter, r *http.Request) {
	var req targetReq
	if !decode(w, r, &req) {
		return
	}
	fs, p, local, err := s.fsFor(r.Context(), req.Target)
	if err == nil {
		err = validPath(local, p)
	}
	if err == nil && p == "" {
		err = vfs.E(vfs.CodeInvalidRequest, "a directory path is required")
	}
	if err == nil {
		err = fs.Mkdir(r.Context(), p)
	}
	done(w, err)
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	var req removeReq
	if !decode(w, r, &req) {
		return
	}
	fs, p, _, err := s.fsFor(r.Context(), req.Target)
	if err == nil && p == "" {
		err = vfs.E(vfs.CodeInvalidRequest, "refusing to remove a share's root")
	}
	if err == nil {
		err = fs.Remove(r.Context(), p, req.Recursive)
	}
	done(w, err)
}

func (s *server) rename(w http.ResponseWriter, r *http.Request) {
	var req renameReq
	if !decode(w, r, &req) {
		return
	}
	fs, p, local, err := s.fsFor(r.Context(), req.Target)
	if err == nil && p == "" {
		err = vfs.E(vfs.CodeInvalidRequest, "refusing to rename a share's root")
	}
	if err == nil {
		err = validName(local, req.NewName)
	}
	if err == nil {
		err = fs.Rename(r.Context(), p, path.Join(path.Dir(p), req.NewName))
	}
	done(w, err)
}

// ---- jobs ----

func (s *server) startJob(w http.ResponseWriter, r *http.Request) {
	var req TransferRequest
	if !decode(w, r, &req) {
		return
	}
	j, err := s.start(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, j)
}

// endpoint resolves a "peer:share/path" to member IDs (R9).
func (s *server) endpoint(spec string) (transfer.Endpoint, bool, error) {
	t, err := ParseTarget(spec)
	if err != nil {
		return transfer.Endpoint{}, false, err
	}
	if t.Share == "" {
		return transfer.Endpoint{}, false, vfs.E(vfs.CodeInvalidRequest, "%q needs a share (peer:share/path)", spec)
	}
	id, local, err := s.Node.ResolvePeer(t.Peer)
	if err != nil {
		return transfer.Endpoint{}, false, err
	}
	return transfer.Endpoint{Peer: id, Share: t.Share, Path: t.Path}, local, nil
}

// start submits a job to its destination host, which runs it.
func (s *server) start(ctx context.Context, req TransferRequest) (transfer.Job, error) {
	if req.Op != "copy" && req.Op != "move" {
		return transfer.Job{}, vfs.E(vfs.CodeInvalidRequest, "op must be copy or move")
	}
	policy := transfer.Conflict(req.OnConflict)
	switch policy {
	case "", transfer.ConflictFail, transfer.ConflictSkip, transfer.ConflictOverwrite, transfer.ConflictRename:
	default:
		return transfer.Job{}, vfs.E(vfs.CodeInvalidRequest, "on-conflict must be fail, skip, overwrite or rename")
	}
	src, _, err := s.endpoint(req.Src)
	if err != nil {
		return transfer.Job{}, err
	}
	dst, dstLocal, err := s.endpoint(req.Dst)
	if err != nil {
		return transfer.Job{}, err
	}
	spec := transfer.Spec{Op: req.Op, Src: src, Dst: dst, OnConflict: policy}
	if dstLocal {
		return s.Jobs.Submit(s.Node.ID(), spec)
	}
	cl, err := s.Node.Route(dst.Peer)
	if err != nil {
		return transfer.Job{}, err
	}
	return cl.SubmitJob(ctx, spec)
}

func (s *server) jobs(w http.ResponseWriter, r *http.Request) {
	out := s.Jobs.List("")
	self := s.Node.ID()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range s.Node.Peers() {
		// Skip hosts known to be down or unusable: waiting for them would
		// stall every refresh. A host never probed yet is still tried.
		if p.Incompatible || (!p.Online && !p.LastSeen.IsZero()) {
			continue
		}
		cl, err := s.Node.Route(p.ID)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), peerJobsTimeout)
			defer cancel()
			js, err := cl.ListJobs(ctx, self)
			if err != nil {
				return // an unreachable peer only costs its own jobs
			}
			mu.Lock()
			out = append(out, js...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.SortStableFunc(out, func(a, b transfer.Job) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	writeJSON(w, 200, out)
}

// jobHost returns the client of the host a job runs on, or nil if that is
// this host.
func (s *server) jobHost(host string) (*peerapi.Client, error) {
	if host == "" || host == s.Node.ID() {
		return nil, nil
	}
	return s.Node.Route(host)
}

func (s *server) job(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl, err := s.jobHost(r.PathValue("host"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if cl == nil {
		j, err := s.Jobs.Get(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, j)
		return
	}
	js, err := cl.ListJobs(r.Context(), "")
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, j := range js {
		if j.ID == id {
			writeJSON(w, 200, j)
			return
		}
	}
	writeErr(w, vfs.E(vfs.CodeNotFound, "no job %q", id))
}

func (s *server) cancelJob(w http.ResponseWriter, r *http.Request) {
	s.jobOp(w, r, s.Jobs.Cancel, (*peerapi.Client).CancelJob)
}

func (s *server) retryJob(w http.ResponseWriter, r *http.Request) {
	s.jobOp(w, r, s.Jobs.Retry, (*peerapi.Client).RetryJob)
}

func (s *server) jobOp(w http.ResponseWriter, r *http.Request, local func(string) error, remote func(*peerapi.Client, context.Context, string) error) {
	id := r.PathValue("id")
	cl, err := s.jobHost(r.PathValue("host"))
	switch {
	case err != nil:
		done(w, err)
	case cl == nil:
		done(w, local(id))
	default:
		done(w, remote(cl, r.Context(), id))
	}
}
