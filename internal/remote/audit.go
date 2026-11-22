package remote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// LogName is the access log in the config directory.
	LogName = "remote.log"
	// logRotateSize: a log larger than this at open is moved to LogName+".1".
	logRotateSize = 10 << 20
)

// Event is one line of the access log. It never holds what went through the
// tunnel, passwords or tokens.
type Event struct {
	Time       time.Time `json:"time"`
	Event      string    `json:"event"` // "open", "close", "denied"
	PeerID     string    `json:"peer_id"`
	PeerName   string    `json:"peer_name,omitempty"`
	RemoteAddr string    `json:"remote_addr,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	BytesIn    int64     `json:"bytes_in,omitempty"`  // controller to target
	BytesOut   int64     `json:"bytes_out,omitempty"` // target to controller
	Seconds    float64   `json:"seconds,omitempty"`
}

// Audit appends events to the access log. Failures to write are reported
// once on stderr and otherwise ignored: a full disk must not lock the owner
// out of their own machine.
type Audit struct {
	path   string
	mu     sync.Mutex
	warned bool
	now    func() time.Time
}

// OpenAudit returns the log in dir, rotating it first if it is large.
func OpenAudit(dir string) *Audit {
	p := filepath.Join(dir, LogName)
	if fi, err := os.Stat(p); err == nil && fi.Size() > logRotateSize {
		os.Rename(p, p+".1")
	}
	return &Audit{path: p, now: time.Now}
}

func (a *Audit) Log(e Event) {
	if a == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = a.now()
	}
	b, _ := json.Marshal(e)
	b = append(b, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil && !a.warned {
		a.warned = true
		os.Stderr.WriteString("remote: cannot write the access log: " + err.Error() + "\n")
	}
}
