package transfer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"sugarintegration/internal/fsutil"
)

const (
	jobFile   = "job.json"
	doneFile  = "done.log"
	partsFile = "parts.log"
)

// record is what job.json holds: the Job and what recovery needs beyond it.
type record struct {
	Job
	// Resolved is the destination after ResolveDst. It is fixed at the first
	// start: resolving again after a restart would see the job's own output
	// and send "a -> x" into x/a.
	Resolved *string `json:",omitempty"`
	// Started is set before the first commit can happen. A job with it set
	// may have been killed between a commit and its done.log entry.
	Started bool `json:",omitempty"`
	// RenameStarted is set before a whole-tree rename: after a kill, a
	// missing source then means the rename happened.
	RenameStarted bool `json:",omitempty"`
	// CancelRequested makes a Cancel survive a kill before it took effect.
	CancelRequested bool `json:",omitempty"`
}

func (m *Manager) jobDir(id string) string { return filepath.Join(m.o.Dir, "jobs", id) }

func writeRecord(dir string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(dir, jobFile), data, 0o600)
}

// loadRecords reads every job under dir. A job that cannot be read is
// skipped: one damaged file must not stop the daemon or the other jobs.
func loadRecords(dir string) ([]record, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []record
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), jobFile))
		if err != nil {
			continue
		}
		var r record
		if json.Unmarshal(b, &r) != nil || r.ID != e.Name() || !r.State.valid() {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// doneEntry is one line of done.log: a committed file. S and M describe the
// destination as committed and E the source version it was verified against,
// so a later source removal can tell that both are still those files. E is
// empty for a file that was renamed in place, whose source no longer exists.
type doneEntry struct {
	P string    `json:"p"` // path relative to the job's source root
	D string    `json:"d"` // final destination path in the share
	S int64     `json:"s"`
	M time.Time `json:"m"`
	E string    `json:"e"`
}

// readDoneLog returns the complete entries by source path. A kill during an
// append leaves an unterminated or unparsable line, which is ignored: the
// crash-window rule recovers that file.
func readDoneLog(path string) (map[string]doneEntry, error) {
	done := map[string]doneEntry{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	for {
		line, rest, complete := bytes.Cut(b, []byte("\n"))
		if !complete {
			return done, nil
		}
		b = rest
		var e doneEntry
		if json.Unmarshal(line, &e) == nil {
			done[e.P] = e
		}
	}
}

// doneLog appends to done.log. Appends are not fsynced: losing the tail in a
// power cut costs nothing, because a lost entry whose source exists is found
// again by the crash-window rule, and one whose source is gone needs nothing.
type doneLog struct{ f *os.File }

func openDoneLog(path string) (*doneLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// Terminate a torn last line so the next entry does not join it.
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err != nil || last[0] != '\n' {
			if _, err := f.Write([]byte("\n")); err != nil {
				f.Close()
				return nil, err
			}
		}
	}
	return &doneLog{f}, nil
}

func (l *doneLog) append(e doneEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = l.f.Write(append(b, '\n'))
	return err
}

func (l *doneLog) close() { l.f.Close() }

// appendPartKey notes that key may hold a part file, so Cancel can find it
// even when the source cannot be walked. Not fsynced: a part orphaned by a
// power cut is left to GCParts.
func appendPartKey(path, key string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(key + "\n")
	return err
}

func readPartKeys(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		keys = append(keys, sc.Text())
	}
	return keys
}
