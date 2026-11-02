package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"sugarintegration/internal/shares"
	"sugarintegration/internal/vfs"
)

const (
	defaultConcurrency = 2
	submitProbeTimeout = 30 * time.Second
	// mtimeSlop absorbs destination filesystems with coarse timestamps (FAT
	// keeps 2s, NTFS 100ns). It only ever gates a SHA-256 comparison.
	mtimeSlop = 2 * time.Second
)

// Options configures a Manager.
type Options struct {
	Dir, SelfID string
	Concurrency int    // below 1 means the default, 2
	GOOS        string // destination OS for name checks; default runtime.GOOS

	Source         func(peerID, share string) (vfs.FS, error)
	SourceReadOnly func(ctx context.Context, peerID, share string) (bool, error)
	Dest           func(share string) (Dest, error)

	RetryMin, RetryMax, SaveEvery time.Duration // default 1s, 60s, 1s
}

// jobState is a job's record plus its in-memory scheduling state.
// Manager.mu guards everything but saveMu.
type jobState struct {
	record
	saveMu     sync.Mutex // serializes job.json writes, taken before Manager.mu
	queued     bool
	cancel     context.CancelFunc // set while an attempt runs
	backoff    time.Duration
	progressed bool
	timerGen   int             // invalidates a pending retry timer
	partKeys   map[string]bool // parts.log in memory; nil until loaded
}

// Manager owns transfer jobs: their queue, persistence and recovery.
type Manager struct {
	o       Options
	mu      sync.Mutex
	jobs    map[string]*jobState
	queue   []*jobState
	running int
	stopped bool
	wake    chan struct{}
	wg      sync.WaitGroup
}

// NewManager loads the jobs under o.Dir and queues the unfinished ones.
func NewManager(o Options) (*Manager, error) {
	if o.Dir == "" || o.Source == nil || o.Dest == nil {
		return nil, vfs.E(vfs.CodeInvalidRequest, "manager needs Dir, Source and Dest")
	}
	if o.Concurrency < 1 {
		o.Concurrency = defaultConcurrency
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.RetryMin <= 0 {
		o.RetryMin = time.Second
	}
	if o.RetryMax <= 0 {
		o.RetryMax = 60 * time.Second
	}
	if o.SaveEvery <= 0 {
		o.SaveEvery = time.Second
	}
	m := &Manager{o: o, jobs: map[string]*jobState{}, wake: make(chan struct{}, 1)}
	dir := filepath.Join(o.Dir, "jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, writeErr(err)
	}
	recs, err := loadRecords(dir)
	if err != nil {
		return nil, writeErr(err)
	}
	slices.SortFunc(recs, func(a, b record) int { return a.Created.Compare(b.Created) })
	for _, r := range recs {
		js := &jobState{record: r}
		js.BytesPerSec = 0
		m.jobs[r.ID] = js
		switch {
		case js.State.terminal():
		case js.CancelRequested:
			m.finishCancel(js)
		default:
			if js.State == StateRunning {
				js.State = StateQueued
			}
			js.partKeys = m.loadPartKeys(js.ID)
			m.enqueueLocked(js)
		}
		if js.State == StateFailed {
			js.partKeys = m.loadPartKeys(r.ID)
		}
	}
	return m, nil
}

// Run executes queued jobs until ctx is done, then waits for running attempts
// to stop. Their jobs stay unfinished on disk and resume in the next Manager.
func (m *Manager) Run(ctx context.Context) error {
	for {
		m.mu.Lock()
		for m.running < m.o.Concurrency && len(m.queue) > 0 && ctx.Err() == nil {
			js := m.queue[0]
			m.queue = m.queue[1:]
			js.queued = false
			m.running++
			m.wg.Add(1)
			attemptCtx, cancel := context.WithCancel(ctx)
			js.cancel = cancel
			go func() {
				defer cancel()
				m.attempt(attemptCtx, js)
			}()
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			m.mu.Lock()
			m.stopped = true
			m.mu.Unlock()
			m.wg.Wait()
			return nil
		case <-m.wake:
		}
	}
}

// Submit accepts a job on this host, the destination. A reachable source is
// validated now and its errors returned; an unreachable one is validated when
// the job starts.
func (m *Manager) Submit(origin string, s Spec) (Job, error) {
	if s.Dst.Peer == "" {
		s.Dst.Peer = m.o.SelfID
	}
	if s.Src.Peer == "" {
		s.Src.Peer = m.o.SelfID
	}
	if s.Dst.Peer != m.o.SelfID {
		return Job{}, vfs.E(vfs.CodeInvalidRequest, "jobs run on their destination host")
	}
	if s.OnConflict == "" {
		s.OnConflict = ConflictFail
	}
	if err := validate(s, false, m.destKey); err != nil {
		return Job{}, err
	}
	dst, err := m.o.Dest(s.Dst.Share)
	if err != nil {
		return Job{}, err
	}
	if dst.ReadOnly() {
		return Job{}, vfs.E(vfs.CodeReadOnly, "destination share is read-only")
	}
	if err := m.probeSource(s, dst); err != nil {
		return Job{}, err
	}

	var rnd [16]byte
	rand.Read(rnd[:])
	now := time.Now()
	js := &jobState{record: record{Job: Job{
		ID: hex.EncodeToString(rnd[:]), Host: m.o.SelfID, Origin: origin, Spec: s,
		State: StateQueued, Created: now, Updated: now,
	}}}
	if err := m.save(js); err != nil {
		return Job{}, writeErr(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[js.ID] = js
	m.enqueueLocked(js)
	return js.Job.clone(), nil
}

// probeSource validates s against the source if it can be reached.
func (m *Manager) probeSource(s Spec, dst Dest) error {
	ctx, cancel := context.WithTimeout(context.Background(), submitProbeTimeout)
	defer cancel()
	err := func() error {
		src, err := m.o.Source(s.Src.Peer, s.Src.Share)
		if err != nil {
			return err
		}
		se, err := src.Stat(ctx, s.Src.Path)
		if err != nil {
			return err
		}
		if err := m.check(ctx, s); err != nil {
			return err
		}
		_, err = m.resolve(ctx, s, se, dst)
		return err
	}()
	if vfs.CodeOf(err) == vfs.CodePeerOffline || ctx.Err() != nil {
		return nil
	}
	return err
}

// List returns jobs oldest first; origin "" means all of them.
func (m *Manager) List(origin string) []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Job{}
	for _, js := range m.jobs {
		if origin == "" || js.Origin == origin {
			out = append(out, js.Job.clone())
		}
	}
	slices.SortFunc(out, func(a, b Job) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Get returns one job.
func (m *Manager) Get(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	js, ok := m.jobs[id]
	if !ok {
		return Job{}, vfs.E(vfs.CodeNotFound, "no job %q", id)
	}
	return js.Job.clone(), nil
}

// ActivePart reports whether key is a part file a job may still resume: one of
// a job that has not finished or of a failed job, which Retry resumes. GCParts
// must leave those alone.
func (m *Manager) ActivePart(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, js := range m.jobs {
		if js.partKeys[key] && (!js.State.terminal() || js.State == StateFailed) {
			return true
		}
	}
	return false
}

// Cancel stops a job and removes its part files. What was already moved stays
// moved. For a job that is mid-attempt this returns at once and the job
// reaches canceled when the attempt has stopped.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	js, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return vfs.E(vfs.CodeNotFound, "no job %q", id)
	}
	if js.State == StateCanceled {
		m.mu.Unlock()
		return nil
	}
	if js.State.terminal() {
		m.mu.Unlock()
		return vfs.E(vfs.CodeInvalidRequest, "job already %s", js.State)
	}
	js.CancelRequested = true
	js.timerGen++
	cancel := js.cancel // nil means no attempt is running or can settle now
	if js.queued {
		js.queued = false
		m.queue = slices.DeleteFunc(m.queue, func(q *jobState) bool { return q == js })
	}
	m.mu.Unlock()
	m.save(js) // best effort: a lost intent means the job resumes after a kill
	if cancel != nil {
		cancel()
		return nil
	}
	m.finishCancel(js)
	return nil
}

// Retry requeues a job that ended failed, partial or canceled. done.log keeps
// its finished items; everything else is attempted again.
func (m *Manager) Retry(id string) error {
	m.mu.Lock()
	js, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return vfs.E(vfs.CodeNotFound, "no job %q", id)
	}
	if !js.State.terminal() || js.State == StateDone {
		defer m.mu.Unlock()
		return vfs.E(vfs.CodeInvalidRequest, "cannot retry a %s job", js.State)
	}
	js.State = StateQueued
	js.Error = nil
	js.CancelRequested = false
	js.backoff = 0
	resetCounters(js)
	m.enqueueLocked(js)
	m.mu.Unlock()
	if err := m.save(js); err != nil {
		return writeErr(err)
	}
	return nil
}

func resetCounters(js *jobState) {
	js.DoneFiles, js.SkippedFiles, js.FailedFiles = 0, 0, 0
	js.DoneBytes, js.BytesPerSec = 0, 0
	js.Failures = nil
}

func (m *Manager) enqueueLocked(js *jobState) {
	if js.queued {
		return
	}
	js.queued = true
	m.queue = append(m.queue, js)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// save writes job.json. It never holds Manager.mu across the disk write.
func (m *Manager) save(js *jobState) error {
	js.saveMu.Lock()
	defer js.saveMu.Unlock()
	m.mu.Lock()
	js.Updated = time.Now()
	b, err := json.Marshal(js.record)
	dir := m.jobDir(js.ID)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return writeRecord(dir, b)
}

// destKey maps a path to what identifies it on the destination filesystem:
// names with one key are one file. On darwin, APFS and HFS+ ignore case and
// Unicode normalization; on windows, NTFS ignores case only. Elsewhere bytes
// decide. The key is for comparison only: names are written as they came.
func (m *Manager) destKey(p string) string {
	switch m.o.GOOS {
	case "darwin":
		return norm.NFC.String(foldCase(p))
	case "windows":
		return foldCase(p)
	}
	return p
}

// foldsNames reports whether different names can be one destination file.
func (m *Manager) foldsNames() bool { return m.o.GOOS == "darwin" || m.o.GOOS == "windows" }

// check validates s against the live state of the source share.
func (m *Manager) check(ctx context.Context, s Spec) error {
	ro := false
	if s.Op == "move" && m.o.SourceReadOnly != nil {
		var err error
		if ro, err = m.o.SourceReadOnly(ctx, s.Src.Peer, s.Src.Share); err != nil {
			return err
		}
	}
	return validate(s, ro, m.destKey)
}

// resolve applies ResolveDst and validates the result. The source's name is
// taken from the requested path, not from what the source reports.
func (m *Manager) resolve(ctx context.Context, s Spec, se vfs.Entry, dst Dest) (string, error) {
	root, err := vfs.Clean(s.Src.Path)
	if err != nil {
		return "", err
	}
	se.Name = ""
	if root != "" {
		se.Name = path.Base(root)
	}
	to, err := ResolveDst(ctx, se, dst, s.Dst.Path)
	if err != nil {
		return "", err
	}
	s.Dst.Path = to
	if err := validate(s, false, m.destKey); err != nil {
		return "", err
	}
	return to, nil
}

// attempt runs a job once and settles its state.
func (m *Manager) attempt(ctx context.Context, js *jobState) {
	defer m.wg.Done()
	stopTicker := m.startTicker(js)
	r := &jobRun{m: m, js: js, ctx: ctx, made: map[string]bool{}, failedDirs: map[string]bool{}}
	err := r.run()
	stopTicker()
	m.settle(ctx, js, err)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// startTicker saves the job and samples its speed every SaveEvery.
func (m *Manager) startTicker(js *jobState) (stop func()) {
	quit, gone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(gone)
		t := time.NewTicker(m.o.SaveEvery)
		defer t.Stop()
		m.mu.Lock()
		last, lastAt := js.DoneBytes, time.Now()
		m.mu.Unlock()
		for {
			select {
			case <-quit:
				return
			case now := <-t.C:
				m.mu.Lock()
				if dt := now.Sub(lastAt).Seconds(); dt > 0 {
					js.BytesPerSec = max(0, int64(float64(js.DoneBytes-last)/dt))
				}
				last, lastAt = js.DoneBytes, now
				m.mu.Unlock()
				m.save(js) // best effort: the next tick or the settle tries again
			}
		}
	}()
	return func() {
		close(quit)
		<-gone
	}
}

// settle decides what an attempt's result means for the job and publishes it
// together with the end of the attempt, so that a Cancel or Retry sees either
// a running attempt or its outcome, never something in between.
func (m *Manager) settle(ctx context.Context, js *jobState, err error) {
	for {
		m.mu.Lock()
		cancelRequested, failedFiles := js.CancelRequested, js.FailedFiles
		m.mu.Unlock()

		var next State
		var nextErr *ItemError
		switch {
		case err == nil:
			next = StateDone
			if failedFiles > 0 {
				next = StatePartial
				m.removeParts(js) // a Retry starts these items over
			}
		case cancelRequested:
			next = StateCanceled
			m.removeParts(js)
		case ctx.Err() != nil:
			next = "" // shutting down: stays unfinished on disk
		case vfs.CodeOf(err) == vfs.CodePeerOffline:
			next = StateWaitingPeer
		default:
			// Parts stay: a failed job is resumed by Retry.
			next = StateFailed
			nextErr = &ItemError{js.Spec.Src.Path, vfs.CodeOf(err), messageOf(err)}
		}

		m.mu.Lock()
		if err != nil && js.CancelRequested != cancelRequested {
			m.mu.Unlock()
			continue // a Cancel arrived while deciding
		}
		js.cancel = nil
		js.BytesPerSec = 0
		m.running--
		if next != "" {
			js.State, js.Error = next, nextErr
		}
		if next == StateWaitingPeer {
			m.armRetryLocked(js)
		}
		m.mu.Unlock()
		m.save(js) // best effort: done.log makes repeating the job harmless
		return
	}
}

func (m *Manager) setState(js *jobState, s State, e *ItemError) {
	m.mu.Lock()
	js.State, js.Error = s, e
	m.mu.Unlock()
	m.save(js) // best effort: done.log makes repeating the job harmless
}

func (m *Manager) finishCancel(js *jobState) {
	m.removeParts(js)
	m.setState(js, StateCanceled, nil)
}

// armRetryLocked schedules the next attempt of a waiting job, which holds no
// slot meanwhile. Backoff is exponential and restarts after an attempt that
// made progress.
func (m *Manager) armRetryLocked(js *jobState) {
	if js.progressed || js.backoff == 0 {
		js.backoff = m.o.RetryMin
	} else {
		js.backoff = min(js.backoff*2, m.o.RetryMax)
	}
	js.progressed = false
	js.timerGen++
	gen := js.timerGen
	time.AfterFunc(js.backoff, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.stopped && js.timerGen == gen && js.State == StateWaitingPeer {
			m.enqueueLocked(js)
		}
	})
}

func (m *Manager) loadPartKeys(id string) map[string]bool {
	keys := map[string]bool{}
	for _, k := range readPartKeys(filepath.Join(m.jobDir(id), partsFile)) {
		keys[k] = true
	}
	return keys
}

// removeParts deletes every part file the job may have left. Errors are left
// for GCParts: there is nothing else to do with them.
func (m *Manager) removeParts(js *jobState) {
	dst, err := m.o.Dest(js.Spec.Dst.Share)
	if err != nil {
		return
	}
	for _, k := range readPartKeys(filepath.Join(m.jobDir(js.ID), partsFile)) {
		dst.RemovePart(k)
	}
}

// jobRun is one attempt at one job.
type jobRun struct {
	m   *Manager
	js  *jobState
	ctx context.Context

	src       vfs.FS
	dst       Dest
	root      string // cleaned source path
	resolved  string // destination of the source root
	recovered bool   // an earlier attempt may have died after a commit
	sameShare bool
	log       *doneLog
	done      map[string]doneEntry
	taken     map[string]bool // case-folded destinations of this job; nil if case matters

	made       map[string]bool // destination directories known to exist
	failedDirs map[string]bool // source dirs with no destination: never removed
}

// run does the work. A nil error means every item was tried and the counters
// hold the outcome; otherwise the job as a whole stopped for that reason.
func (r *jobRun) run() (err error) {
	m, js := r.m, r.js
	spec := js.Spec // Spec never changes once submitted
	if r.dst, err = m.o.Dest(spec.Dst.Share); err != nil {
		return err
	}
	if r.dst.ReadOnly() {
		return vfs.E(vfs.CodeReadOnly, "destination share is read-only")
	}
	if r.src, err = m.o.Source(spec.Src.Peer, spec.Src.Share); err != nil {
		return err
	}
	if r.root, err = vfs.Clean(spec.Src.Path); err != nil {
		return err
	}
	r.sameShare = spec.Src.Peer == spec.Dst.Peer && spec.Src.Share == spec.Dst.Share

	dir := m.jobDir(js.ID)
	if r.done, err = readDoneLog(filepath.Join(dir, doneFile)); err != nil {
		return writeErr(err)
	}

	se, err := r.src.Stat(r.ctx, r.root)
	gone := vfs.CodeOf(err) == vfs.CodeNotFound
	m.mu.Lock()
	resolved := js.Resolved
	renameStarted := js.RenameStarted
	m.mu.Unlock()
	switch {
	case gone && resolved == nil:
		return err
	case gone && renameStarted:
		// A kill right after the whole-tree rename.
		if _, serr := r.dst.Stat(r.ctx, *resolved); serr != nil {
			return err
		}
		m.mu.Lock()
		resetCounters(js)
		js.DoneFiles, js.DoneBytes = js.TotalFiles, js.TotalBytes
		m.mu.Unlock()
		return nil
	case gone && (len(r.done) == 0 || spec.Op != "move"):
		return err // only a move explains a source that is gone
	case err != nil && !gone:
		return err
	}
	if !gone {
		if err := m.check(r.ctx, spec); err != nil {
			return err
		}
	}
	if resolved == nil {
		to, err := m.resolve(r.ctx, spec, se, r.dst)
		if err != nil {
			return err
		}
		resolved = &to
	}
	r.resolved = *resolved

	// Persist the destination and the started mark before anything can be
	// committed: recovery relies on both.
	m.mu.Lock()
	r.recovered = js.Started
	js.Started = true
	js.Resolved = resolved
	js.State = StateRunning
	js.Error = nil
	resetCounters(js)
	m.mu.Unlock()
	if err := m.save(js); err != nil {
		return writeErr(err)
	}

	pl := &plan{}
	if !gone {
		if pl, err = buildPlan(r.ctx, r.src, r.root, se); err != nil {
			return err
		}
	}
	r.applyPlanTotals(pl)
	if err := r.ctx.Err(); err != nil {
		return canceled()
	}

	if renamed, err := r.tryWholeRename(pl, spec); renamed || err != nil {
		return err
	}

	if r.log, err = openDoneLog(filepath.Join(dir, doneFile)); err != nil {
		return writeErr(err)
	}
	defer r.log.close()
	keys := m.loadPartKeys(js.ID)
	m.mu.Lock()
	js.partKeys = keys
	m.mu.Unlock()
	if m.foldsNames() {
		r.taken = make(map[string]bool, len(r.done))
		for _, e := range r.done {
			r.take(e.D)
		}
	}

	for _, d := range pl.dirs {
		if err := r.makeDir(d); err != nil {
			return err
		}
	}
	for _, f := range pl.files {
		if r.ctx.Err() != nil {
			return canceled()
		}
		if err := r.file(f); err != nil {
			return err
		}
	}
	if spec.Op == "move" {
		return r.removeEmptyDirs(pl)
	}
	return nil
}

// applyPlanTotals sets the counters a fresh walk determines. Files already in
// done.log count as done; a moved one no longer shows up in the walk at all.
func (r *jobRun) applyPlanTotals(pl *plan) {
	inPlan := make(map[string]bool, len(pl.files))
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	js := r.js
	js.TotalFiles, js.TotalBytes = len(pl.files), 0
	js.SkippedFiles = pl.skipped
	for _, f := range pl.files {
		inPlan[f.rel] = true
		js.TotalBytes += f.size
	}
	for rel, e := range r.done {
		if !inPlan[rel] {
			js.TotalFiles++
			js.TotalBytes += e.S
			js.DoneFiles++
			js.DoneBytes += e.S
		}
	}
	for _, f := range pl.failures {
		r.recordLocked(f)
	}
}

func (r *jobRun) recordLocked(f ItemError) {
	r.js.FailedFiles++
	if len(r.js.Failures) < maxFailures {
		r.js.Failures = append(r.js.Failures, f)
	}
}

// fail settles an error on one item. Item errors are recorded and the job
// goes on; the returned error stops the job.
func (r *jobRun) fail(p string, err error) error {
	if r.ctx.Err() != nil {
		return canceled()
	}
	switch vfs.CodeOf(err) {
	case vfs.CodePeerOffline, vfs.CodeDiskFull:
		return err
	}
	r.m.mu.Lock()
	r.recordLocked(ItemError{p, vfs.CodeOf(err), messageOf(err)})
	r.m.mu.Unlock()
	return nil
}

func (r *jobRun) srcPath(rel string) string { return path.Join(r.root, rel) }
func (r *jobRun) dstPath(rel string) string { return path.Join(r.resolved, rel) }

// tryWholeRename moves the whole source with one atomic rename when that is
// the same as moving it item by item: same share, nothing at the destination
// and nothing to skip or fail. Otherwise, or if the rename fails, the job goes
// item by item, where the conflict policy and failures apply to each file.
func (r *jobRun) tryWholeRename(pl *plan, spec Spec) (bool, error) {
	if !r.sameShare || spec.Op != "move" || r.root == "" || r.resolved == "" ||
		len(r.done) > 0 || pl.skipped > 0 || len(pl.failures) > 0 {
		return false, nil
	}
	if r.checkDst(r.resolved) != nil {
		return false, nil
	}
	for _, f := range pl.files {
		if r.checkDst(r.dstPath(f.rel)) != nil {
			return false, nil
		}
	}
	for _, d := range pl.dirs {
		if r.checkDst(r.dstPath(d.rel)) != nil {
			return false, nil
		}
	}
	if _, err := r.dst.Stat(r.ctx, r.resolved); vfs.CodeOf(err) != vfs.CodeNotFound {
		return false, nil
	}
	if r.ensureDir(parentOf(r.resolved)) != nil {
		return false, nil
	}
	m, js := r.m, r.js
	m.mu.Lock()
	js.RenameStarted = true
	m.mu.Unlock()
	if err := m.save(js); err != nil {
		return false, writeErr(err)
	}
	err := r.dst.Rename(r.ctx, r.root, r.resolved)
	if r.ctx.Err() != nil {
		return false, canceled()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		js.RenameStarted = false
		return false, nil
	}
	js.DoneFiles, js.DoneBytes = js.TotalFiles, js.TotalBytes
	return true, nil
}

// messageOf is the text of err without its code, which is reported apart.
func messageOf(err error) string {
	var ve *vfs.Error
	if errors.As(err, &ve) {
		return ve.Message
	}
	return err.Error()
}

// foldCase maps paths that differ only by case to one string. Lowering alone
// misses letters such as long s, which only meet their pair through upper case.
func foldCase(p string) string { return strings.ToLower(strings.ToUpper(p)) }

func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// checkDst rejects a destination path with a component the destination OS
// cannot create. Fetch only checks the last one.
func (r *jobRun) checkDst(p string) error {
	clean, err := vfs.Clean(p)
	if err != nil {
		return err
	}
	if clean == "" {
		return nil
	}
	for _, part := range strings.Split(clean, "/") {
		if !shares.ValidName(r.m.o.GOOS, part) {
			return vfs.E(vfs.CodeInvalidName, "name %q cannot be created on %s", part, r.m.o.GOOS)
		}
	}
	return nil
}

// ensureDir makes directory p and its parents. Anything but a real directory
// in the way is a failure: nothing is written through a link.
func (r *jobRun) ensureDir(p string) error {
	if p == "" || r.made[p] {
		return nil
	}
	if err := r.checkDst(p); err != nil {
		return err
	}
	cur := ""
	for _, part := range strings.Split(p, "/") {
		cur = path.Join(cur, part)
		if r.made[cur] {
			continue
		}
		e, err := r.dst.Stat(r.ctx, cur)
		if vfs.CodeOf(err) == vfs.CodeNotFound {
			err = r.dst.Mkdir(r.ctx, cur)
			if err == nil {
				r.made[cur] = true
				continue
			}
			if vfs.CodeOf(err) != vfs.CodeExists {
				return err
			}
			e, err = r.dst.Stat(r.ctx, cur) // lost a race: see what is there now
		}
		switch {
		case err != nil:
			return err
		case e.Type == vfs.TypeSymlink:
			return vfs.E(vfs.CodeInvalidPath, "%q is a symbolic link", cur)
		case e.Type != vfs.TypeDir:
			return vfs.E(vfs.CodeConflict, "%q exists and is not a directory", cur)
		}
		r.made[cur] = true
	}
	return nil
}

// makeDir creates one walked directory. A non-empty directory that fails is
// not itself an item: each file under it fails on its own.
func (r *jobRun) makeDir(d planDir) error {
	dstP := r.dstPath(d.rel)
	var err error
	if r.inOwnSource(dstP) {
		err = vfs.E(vfs.CodeConflict, "destination %q is inside this job's source", dstP)
	} else {
		err = r.ensureDir(dstP)
	}
	if err == nil {
		return nil
	}
	r.failedDirs[d.rel] = true
	if !d.empty && r.ctx.Err() == nil && vfs.CodeOf(err) != vfs.CodeDiskFull {
		return nil
	}
	return r.fail(r.srcPath(d.rel), err)
}

// inOwnSource reports whether p is at or under this job's source root. That
// passes validation when the destination is an ancestor of the source: a/a
// into the root resolves to a, and a/a/a/x then lands on a/a/x, the job's own
// source of another item. No policy may write there.
func (r *jobRun) inOwnSource(p string) bool {
	if !r.sameShare {
		return false
	}
	root, p := r.m.destKey(r.root), r.m.destKey(p)
	return p == root || strings.HasPrefix(p, root+"/")
}

// take remembers a destination this job wrote, so that a second item whose
// name is the same file on this filesystem cannot replace it.
func (r *jobRun) take(p string) {
	if r.taken != nil {
		r.taken[r.m.destKey(p)] = true
	}
}

// file transfers one file.
func (r *jobRun) file(f planFile) error {
	srcP, dstP := r.srcPath(f.rel), r.dstPath(f.rel)
	if e, ok := r.done[f.rel]; ok {
		return r.finishLogged(f, e, 0)
	}
	if err := r.checkDst(dstP); err != nil {
		return r.fail(srcP, err)
	}
	if r.inOwnSource(dstP) {
		return r.fail(srcP, vfs.E(vfs.CodeConflict, "destination %q is inside this job's source", dstP))
	}
	if r.taken[r.m.destKey(dstP)] {
		return r.fail(srcP, vfs.E(vfs.CodeConflict, "destination %q is the same name as a file this job already wrote, apart from case or normalization", dstP))
	}
	if err := r.ensureDir(parentOf(dstP)); err != nil {
		return r.fail(srcP, err)
	}
	key := partKey(r.js.ID, f.rel)
	if r.recovered {
		e, found, err := r.findCommitted(f, srcP, dstP)
		if err != nil {
			return r.fail(srcP, err)
		}
		if found {
			r.dst.RemovePart(key) // a commit can die before clearing the etag
			r.take(e.D)
			return r.commitDone(f, e, 0)
		}
	}
	if r.sameShare && r.js.Spec.Op == "move" {
		renamed, err := r.renameFile(f, srcP, dstP)
		if renamed || err != nil {
			return err
		}
	}
	return r.fetch(f, srcP, dstP, key)
}

// renameFile moves one file inside the share when its name is free. A taken
// name returns false: Fetch then applies the conflict policy, and overwrite
// replaces the old file atomically only after the new one is verified.
func (r *jobRun) renameFile(f planFile, srcP, dstP string) (bool, error) {
	err := r.dst.Rename(r.ctx, srcP, dstP) // refuses a taken name
	if vfs.CodeOf(err) == vfs.CodeExists {
		return false, nil
	}
	if err != nil {
		return false, r.fail(srcP, err)
	}
	r.take(dstP)
	r.addDone(f, f.size, 0)
	// The source is gone, so a lost entry costs nothing.
	if err := r.log.append(doneEntry{P: f.rel, D: dstP, S: f.size}); err != nil && shares.IsDiskFull(err) {
		return true, writeErr(err)
	}
	return true, nil
}

// addDone counts one finished file of the given real size, held bytes of
// which DoneBytes already includes.
func (r *jobRun) addDone(f planFile, size, held int64) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	r.js.DoneFiles++
	r.js.DoneBytes += size - held
	r.js.TotalBytes += size - f.size
	r.js.progressed = true
}

func (r *jobRun) fetch(f planFile, srcP, dstP, key string) error {
	m, js := r.m, r.js
	m.mu.Lock()
	known := js.partKeys[key]
	m.mu.Unlock()
	if !known {
		if err := appendPartKey(filepath.Join(m.jobDir(js.ID), partsFile), key); err != nil {
			return r.fail(srcP, writeErr(err))
		}
		m.mu.Lock()
		js.partKeys[key] = true
		m.mu.Unlock()
	}
	held := int64(0)
	res, err := Fetch(r.ctx, r.src, srcP, r.dst, dstP, key, js.Spec.OnConflict, m.o.GOOS, func(d int64) {
		held += d
		m.mu.Lock()
		js.DoneBytes += d
		if d > 0 {
			js.progressed = true
		}
		m.mu.Unlock()
	})
	if err != nil || res.Skipped {
		// Whatever this call reported is not done; a resume reports it again.
		m.mu.Lock()
		js.DoneBytes -= held
		if err == nil {
			js.SkippedFiles++
			js.TotalBytes -= f.size
		}
		m.mu.Unlock()
		if err != nil {
			return r.fail(srcP, err)
		}
		return nil
	}
	r.take(res.Dst)
	de, err := r.dst.Stat(r.ctx, res.Dst)
	if err != nil {
		return r.fail(srcP, err)
	}
	return r.commitDone(f, doneEntry{P: f.rel, D: res.Dst, S: de.Size, M: de.ModTime, E: res.SrcETag}, held)
}

// commitDone runs the steps after a verified commit, in the order that makes
// a kill between any two of them recoverable: log it, then remove the source.
// held is how many of the file's bytes DoneBytes already includes.
func (r *jobRun) commitDone(f planFile, e doneEntry, held int64) error {
	if err := r.log.append(e); err != nil {
		return r.fail(r.srcPath(f.rel), writeErr(err)) // source kept
	}
	return r.finishLogged(f, e, held)
}

// finishLogged completes a file that is in done.log. For a move that means
// removing a source that may have outlived the entry.
func (r *jobRun) finishLogged(f planFile, e doneEntry, held int64) error {
	if r.js.Spec.Op == "move" {
		removed, err := r.removeSource(r.srcPath(f.rel), e)
		if !removed {
			return err
		}
	}
	r.addDone(f, e.S, held)
	return nil
}

// removeSource deletes a logged file's source, but only while the source is
// still the version that was verified and the destination still the file that
// was committed; otherwise the source may be the only copy of something.
func (r *jobRun) removeSource(srcP string, e doneEntry) (bool, error) {
	se, err := r.src.Stat(r.ctx, srcP)
	if vfs.CodeOf(err) == vfs.CodeNotFound {
		return true, nil
	}
	if err != nil {
		return false, r.fail(srcP, err)
	}
	if se.Type != vfs.TypeFile || se.ETag != e.E {
		return false, r.fail(srcP, vfs.E(vfs.CodeChanged, "source changed after it was transferred; keeping it"))
	}
	de, err := r.dst.Stat(r.ctx, e.D)
	if err != nil && vfs.CodeOf(err) != vfs.CodeNotFound {
		return false, r.fail(srcP, err)
	}
	if err != nil || de.Type != vfs.TypeFile || de.Size != e.S || !de.ModTime.Equal(e.M) {
		return false, r.fail(srcP, vfs.E(vfs.CodeChanged, "destination %q changed after the transfer; keeping the source", e.D))
	}
	err = r.src.Remove(r.ctx, srcP, false)
	if err != nil && vfs.CodeOf(err) != vfs.CodeNotFound {
		return false, r.fail(srcP, err)
	}
	return true, nil
}

// findCommitted is the crash-window rule. On a job that may have been killed
// between CommitPart and the done.log append, a destination file with the
// source's size and mtime and the same SHA-256 is that commit, whatever the
// conflict policy would say about it. Under the rename policy the commit may
// sit under any of the numbered names, so those are searched too.
func (r *jobRun) findCommitted(f planFile, srcP, dstP string) (doneEntry, bool, error) {
	se, err := r.src.Stat(r.ctx, srcP)
	if vfs.CodeOf(err) == vfs.CodeNotFound {
		return doneEntry{}, false, nil // Fetch reports it
	}
	if err != nil {
		return doneEntry{}, false, err
	}
	if se.Type != vfs.TypeFile {
		return doneEntry{}, false, nil
	}
	tries := 1
	if r.js.Spec.OnConflict == ConflictRename {
		tries = maxNameTries
	}
	srcSum := ""
	for i := range tries {
		cand := dstP
		if i > 0 {
			cand = numberedName(dstP, i)
		}
		de, err := r.dst.Stat(r.ctx, cand)
		if vfs.CodeOf(err) == vfs.CodeNotFound {
			break // conflict names are taken in order, so the search ends here
		}
		if err != nil {
			return doneEntry{}, false, err
		}
		if de.Type != vfs.TypeFile || de.Size != se.Size || de.ModTime.Sub(se.ModTime).Abs() > mtimeSlop {
			continue
		}
		if srcSum == "" {
			if srcSum, err = r.src.Hash(r.ctx, srcP); err != nil {
				return doneEntry{}, false, err
			}
		}
		dstSum, err := r.dst.Hash(r.ctx, cand)
		if err != nil {
			return doneEntry{}, false, err
		}
		if dstSum != srcSum {
			continue
		}
		after, err := r.src.Stat(r.ctx, srcP)
		if err != nil {
			return doneEntry{}, false, err
		}
		if after.ETag != se.ETag {
			return doneEntry{}, false, nil // changed while hashing: not the same version
		}
		return doneEntry{P: f.rel, D: cand, S: de.Size, M: de.ModTime, E: se.ETag}, true, nil
	}
	return doneEntry{}, false, nil
}

// removeEmptyDirs removes the walked source directories deepest first, never
// recursively: a directory that still holds a failed, skipped or new file is
// left alone, and so is one that could not be created at the destination.
func (r *jobRun) removeEmptyDirs(pl *plan) error {
	for _, d := range slices.Backward(pl.dirs) {
		p := r.srcPath(d.rel)
		if p == "" || r.failedDirs[d.rel] {
			continue
		}
		err := r.src.Remove(r.ctx, p, false)
		switch vfs.CodeOf(err) {
		case "", vfs.CodeNotFound, vfs.CodeNotEmpty:
		default:
			if err := r.fail(p, err); err != nil {
				return err
			}
		}
	}
	return nil
}
