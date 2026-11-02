// Package transfer moves files between shares: resumable, verified, and
// never committed unverified.
package transfer

import (
	"os"
	"time"

	"sugarintegration/internal/vfs"
)

// Dest is the destination side of a transfer: a share filesystem that can
// also hold resumable partial files.
type Dest interface {
	vfs.FS
	ReadOnly() bool
	OpenPart(key string) (*os.File, int64, error)
	PartETag(key string) (string, error)
	SetPartETag(key, etag string) error
	ResetPart(key string) error
	RemovePart(key string) error
	CommitPart(key, dst string, mtime time.Time, mode uint32) error
	SyncParent(p string) error
}

// Conflict is the policy applied when the destination name is taken.
type Conflict string

const (
	ConflictFail      Conflict = "fail"
	ConflictSkip      Conflict = "skip"
	ConflictOverwrite Conflict = "overwrite"
	ConflictRename    Conflict = "rename"
)

// Endpoint names a path inside a share on a peer. Peer is a member ID.
type Endpoint struct{ Peer, Share, Path string }

// Spec describes a transfer job. Op is "copy" or "move".
type Spec struct {
	Op         string
	Src, Dst   Endpoint
	OnConflict Conflict
}

// FetchResult reports where the file went, or why it was not transferred.
type FetchResult struct {
	Dst        string
	SrcETag    string // the source version that was verified and committed
	Skipped    bool
	SkipReason string
}

// Skip reasons.
const (
	SkippedSymlink = "skipped_symlink"
	SkippedExists  = "skipped_exists"
)
