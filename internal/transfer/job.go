package transfer

import (
	"slices"
	"time"
)

// State is a job's position in its lifecycle:
// queued -> running <-> waiting_peer -> done | partial | failed | canceled.
type State string

const (
	StateQueued      State = "queued"
	StateRunning     State = "running"
	StateWaitingPeer State = "waiting_peer"
	StateDone        State = "done"
	StatePartial     State = "partial"
	StateFailed      State = "failed"
	StateCanceled    State = "canceled"
)

// maxFailures caps Job.Failures; FailedFiles keeps counting past it.
const maxFailures = 1000

func (s State) valid() bool {
	switch s {
	case StateQueued, StateRunning, StateWaitingPeer, StateDone, StatePartial, StateFailed, StateCanceled:
		return true
	}
	return false
}

// terminal reports whether the job has stopped for good until a Retry.
func (s State) terminal() bool {
	switch s {
	case StateDone, StatePartial, StateFailed, StateCanceled:
		return true
	}
	return false
}

// ItemError says why one path, or the whole job, failed. Code is a spec code.
type ItemError struct{ Path, Code, Message string }

// Job is a snapshot of one transfer. It runs on the destination host (Host).
type Job struct {
	ID, Host, Origin string
	Spec             Spec
	State            State
	Error            *ItemError // why the job as a whole failed

	TotalFiles, DoneFiles, SkippedFiles, FailedFiles int
	TotalBytes, DoneBytes, BytesPerSec               int64

	Failures []ItemError // capped at maxFailures

	Created, Updated time.Time
}

// clone returns a copy that shares nothing mutable with j.
func (j Job) clone() Job {
	j.Failures = slices.Clone(j.Failures)
	if j.Error != nil {
		e := *j.Error
		j.Error = &e
	}
	return j
}
