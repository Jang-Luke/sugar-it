// Package daemon wires the node, the transfer manager and the three
// listeners (Peer API, Control API, WebDAV) into one running host.
package daemon

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"sugarintegration/internal/config"
	"sugarintegration/internal/control"
	"sugarintegration/internal/identity"
	"sugarintegration/internal/node"
	"sugarintegration/internal/peerapi"
	"sugarintegration/internal/shares"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

const (
	partMaxAge    = 7 * 24 * time.Hour
	defaultGrace  = 10 * time.Second // how long Shutdown lets open requests finish
	davNotReadyMS = "WebDAV is not available in this build yet"
)

// Options configures Start. An empty address means the configured one.
type Options struct {
	ConfigDir, ListenAddr, ControlAddr, DAVAddr string
	InProcess                                   bool // hosted by the desktop app
}

// Daemon is a running host.
type Daemon struct {
	dir  string
	n    *node.Node
	jobs *transfer.Manager
	rt   control.Runtime

	lns  [3]net.Listener // control, peer, dav
	srvs [3]*http.Server

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	serveErr error
	stopOnce sync.Once
	stopped  chan struct{} // closed when Shutdown has finished
}

func listen(kind, addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, vfs.E(vfs.CodeInternal, "cannot listen on %s for the %s API: %v", addr, kind, err)
	}
	return ln, nil
}

// Start binds the listeners and starts serving. It returns once everything is
// bound. Control binds first, then Peer, then WebDAV, so a second daemon on
// the same configuration fails on the Control port before touching anything.
func Start(ctx context.Context, o Options) (*Daemon, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := o.ConfigDir
	if dir == "" {
		var err error
		if dir, err = config.DefaultDir(); err != nil {
			return nil, err
		}
	}
	cfgStore, err := config.Open(dir)
	if err != nil {
		return nil, err
	}
	cfg := cfgStore.Get()
	token, err := config.ControlToken(dir)
	if err != nil {
		return nil, err
	}
	pick := func(override, configured string) string {
		if override != "" {
			return override
		}
		return configured
	}

	d := &Daemon{dir: dir, stopped: make(chan struct{})}
	ok := false
	defer func() {
		if !ok {
			for _, ln := range d.lns {
				if ln != nil {
					ln.Close()
				}
			}
		}
	}()
	for i, b := range []struct{ kind, addr string }{
		{"control", pick(o.ControlAddr, cfg.ControlAddr)},
		{"peer", pick(o.ListenAddr, cfg.ListenAddr)},
		{"dav", pick(o.DAVAddr, cfg.DAVAddr)},
	} {
		if d.lns[i], err = listen(b.kind, b.addr); err != nil {
			return nil, err
		}
	}

	id, err := identity.LoadOrCreate(dir)
	if err != nil {
		return nil, err
	}
	d.n, err = node.New(cfgStore, id)
	if err != nil {
		return nil, err
	}
	// A configured port of 0 (or an override) must not leak into the
	// addresses other hosts are told to dial.
	if port, err := portOf(d.lns[1]); err == nil {
		if err := d.n.SetPeerPort(port); err != nil {
			return nil, err
		}
	}
	d.jobs, err = transfer.NewManager(transfer.Options{
		Dir: dir, SelfID: id.ID, Concurrency: cfg.MaxConcurrentJobs,
		Source: d.n.SourceFS, SourceReadOnly: d.n.SourceReadOnly, Dest: d.n.DestFS,
	})
	if err != nil {
		return nil, err
	}
	d.n.AttachJobs(d.jobs) // before the peer listener accepts
	d.collectParts(cfg.Shares)

	d.srvs[0] = control.NewServer(control.Deps{Node: d.n, Jobs: d.jobs, Token: token, InProcess: o.InProcess})
	d.srvs[1] = peerapi.NewServer(id, d.n.Backend())
	d.srvs[2] = &http.Server{Handler: davHandler(d.n, token), ReadHeaderTimeout: 10 * time.Second}

	runCtx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.rt = control.Runtime{ControlAddr: d.lns[0].Addr().String(), PeerAddr: d.lns[1].Addr().String(),
		DAVAddr: d.lns[2].Addr().String(), PID: os.Getpid()}
	if err := control.WriteRuntime(dir, d.rt); err != nil {
		cancel()
		return nil, err
	}
	ok = true

	d.goRun(func() { d.n.Run(runCtx) })
	d.goRun(func() { d.jobs.Run(runCtx) })
	for i := range d.srvs {
		d.goRun(func() {
			var err error
			if i == 1 {
				err = d.srvs[i].ServeTLS(d.lns[i], "", "")
			} else {
				err = d.srvs[i].Serve(d.lns[i])
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				d.fail(err)
			}
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			d.Shutdown(context.Background())
		case <-d.stopped:
		}
	}()
	return d, nil
}

func portOf(ln net.Listener) (int, error) {
	_, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(p)
}

func (d *Daemon) goRun(f func()) {
	d.wg.Add(1)
	go func() { defer d.wg.Done(); f() }()
}

// fail records a listener that died on its own and stops the daemon.
func (d *Daemon) fail(err error) {
	d.mu.Lock()
	if d.serveErr == nil {
		d.serveErr = err
	}
	d.mu.Unlock()
	go d.Shutdown(context.Background())
}

// collectParts removes part files older than 7 days that no job may resume.
// A share that cannot be opened is skipped.
func (d *Daemon) collectParts(specs []shares.Spec) {
	for _, s := range specs {
		l, err := shares.Open(s)
		if err != nil {
			log.Printf("sugar-it: skipping part cleanup of share %q: %v", s.Name, err)
			continue
		}
		if err := l.GCParts(partMaxAge, d.jobs.ActivePart); err != nil {
			log.Printf("sugar-it: part cleanup of share %q: %v", s.Name, err)
		}
		l.Close()
	}
}

// davHandler serves WebDAV; Task 10 replaces it.
func davHandler(n *node.Node, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, davNotReadyMS, http.StatusServiceUnavailable)
	})
}

// PeerAddr is the address the Peer API is bound to.
func (d *Daemon) PeerAddr() string { return d.rt.PeerAddr }

// ControlAddr is the address the Control API is bound to.
func (d *Daemon) ControlAddr() string { return d.rt.ControlAddr }

// DAVAddr is the address the WebDAV listener is bound to.
func (d *Daemon) DAVAddr() string { return d.rt.DAVAddr }

// Node is the running node.
func (d *Daemon) Node() *node.Node { return d.n }

// Wait blocks until the daemon has stopped, whether by Shutdown, by its
// context, or because a listener failed, and returns that failure if any.
func (d *Daemon) Wait() error {
	<-d.stopped
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.serveErr
}

// Shutdown stops accepting connections, lets open requests finish until ctx
// ends (or 10s if ctx has no deadline), stops the node and the transfer
// manager, closes everything and releases the ports. Running jobs keep their
// state on disk and resume at the next start; they are not marked failed.
// Calling it again waits for the first call to finish.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.stopOnce.Do(func() {
		defer close(d.stopped)
		d.cancel() // node and manager: running attempts stop, jobs stay unfinished
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, defaultGrace)
			defer cancel()
		}
		var sw sync.WaitGroup
		for _, s := range d.srvs {
			sw.Add(1)
			go func() {
				defer sw.Done()
				if s.Shutdown(ctx) != nil {
					s.Close()
				}
			}()
		}
		sw.Wait()
		for _, ln := range d.lns {
			ln.Close()
		}
		d.wg.Wait()
		d.n.Close()
		control.RemoveRuntime(d.dir, d.rt)
	})
	select {
	case <-d.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
