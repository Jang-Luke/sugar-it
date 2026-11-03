// Package control is the loopback Control API: the server the daemon runs
// and the client the CLI and GUI use to talk to it.
package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"sugarintegration/internal/fsutil"
	"sugarintegration/internal/mesh"
	"sugarintegration/internal/node"
	"sugarintegration/internal/vfs"
)

// Version is reported by Status. Release builds set it with -ldflags.
var Version = "dev"

// ErrUnavailable means no daemon answers for a config directory.
var ErrUnavailable = vfs.E(vfs.CodeInternal, "no sugar-it daemon is running")

// Status describes the daemon and what it knows of the mesh.
type Status struct {
	ID, Name  string
	Peers     []node.PeerStatus
	Version   string
	InProcess bool // the daemon lives inside the desktop app: closing it stops sharing
}

// TransferRequest asks for a copy or move. Src and Dst are "peer:share/path".
type TransferRequest struct {
	Op         string // "copy" or "move"
	Src, Dst   string
	OnConflict string // "", "fail", "skip", "overwrite" or "rename"
}

// Target is a parsed "peer:share/path".
type Target struct{ Peer, Share, Path string }

// ParseTarget splits "peer:share/path". The share and path may be missing.
// Everything after the first "/" is the path, kept as written so that a
// trailing "/" (a destination directory) survives.
func ParseTarget(s string) (Target, error) {
	peer, rest, hasRest := strings.Cut(s, ":")
	if !validPeer(peer) {
		return Target{}, vfs.E(vfs.CodeInvalidRequest, "%q: the host name must be a-z, 0-9, - (1-32 chars) or local", s)
	}
	t := Target{Peer: peer}
	if !hasRest {
		return t, nil
	}
	share, path, _ := strings.Cut(rest, "/")
	if !mesh.ValidName(share) {
		return Target{}, vfs.E(vfs.CodeInvalidRequest, "%q: the share name must be a-z, 0-9, - (1-32 chars)", s)
	}
	t.Share, t.Path = share, path
	return t, nil
}

func validPeer(name string) bool { return name == mesh.ReservedName || mesh.ValidName(name) }

// ---- wire types ----

type (
	inviteReq  struct{ TTLSeconds int64 }
	inviteResp struct{ Invite string }
	joinReq    struct{ Invite string }
	nameReq    struct{ Name string }
	targetReq  struct{ Target Target }
	removeReq  struct {
		Target    Target
		Recursive bool
	}
	renameReq struct {
		Target  Target
		NewName string
	}
)

// ---- runtime.json ----

// RuntimeFile is written into the config directory while a daemon runs.
// It holds addresses only: never the token.
const RuntimeFile = "runtime.json"

// Runtime is the content of runtime.json: the addresses actually bound.
type Runtime struct {
	ControlAddr, DAVAddr, PeerAddr string
	PID                            int
}

// WriteRuntime stores rt in dir atomically.
func WriteRuntime(dir string, rt Runtime) error {
	b, err := json.MarshalIndent(rt, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(dir, RuntimeFile), append(b, '\n'), 0o600)
}

// RemoveRuntime deletes dir's runtime.json if it still describes rt, so that a
// daemon never removes the file of one that replaced it.
func RemoveRuntime(dir string, rt Runtime) {
	cur, err := readRuntime(dir)
	if err == nil && cur == rt {
		os.Remove(filepath.Join(dir, RuntimeFile))
	}
}

func readRuntime(dir string) (Runtime, error) {
	var rt Runtime
	b, err := os.ReadFile(filepath.Join(dir, RuntimeFile))
	if err != nil {
		return rt, err
	}
	if err := json.Unmarshal(b, &rt); err != nil {
		return rt, err
	}
	if rt.ControlAddr == "" {
		return rt, errors.New("runtime.json has no control address")
	}
	return rt, nil
}
