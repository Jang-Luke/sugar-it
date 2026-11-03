// Package peerapi is the mTLS HTTP/2 API hosts use to talk to each other
// (spec section 6): the server side, a pinned client, and RemoteFS, a vfs.FS
// over the wire.
package peerapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"sugarintegration/internal/mesh"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

// ProtocolMajor is the wire protocol's major version.
const ProtocolMajor = 1

type Info struct {
	ID, Name string
	Protocol int
}

// ShareInfo is all a peer learns about a share: never its local path.
type ShareInfo struct {
	Name     string
	ReadOnly bool
}

type PairRequest struct {
	Token, Name string
	Addrs       []string
}

type PairResponse struct {
	Self    mesh.Member
	Members []mesh.Member
}

// Backend is what the server needs from the daemon. callerID is always the
// ID of the TLS client certificate, never a value the caller claims.
type Backend interface {
	IsMember(id string) bool
	// CallerContext is done when the member id is removed, ending everything
	// it has in flight, and from the start if id is no longer a member.
	CallerContext(id string) context.Context
	Info() Info
	Shares() []ShareInfo
	ShareFS(name string) (vfs.FS, error)
	Pair(ctx context.Context, callerID string, r PairRequest) (PairResponse, error)
	SyncMembers(ctx context.Context, callerID string, remote []mesh.Member) ([]mesh.Member, error)
	SubmitJob(callerID string, s transfer.Spec) (transfer.Job, error)
	ListJobs(origin string) []transfer.Job
	CancelJob(id string) error
	RetryJob(id string) error
}

// maxJSONBody bounds every JSON request body. File uploads are not bounded.
const maxJSONBody = 4 << 20

// Request and response bodies that are not plain spec types.
type (
	pathReq   struct{ Path string }
	removeReq struct {
		Path      string
		Recursive bool
	}
	renameReq struct{ From, To string }
	hashBody  struct {
		SHA256  string `json:"sha256,omitempty"`
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	}
)

// entryHeader carries the opened file's metadata next to its bytes.
const entryHeader = "X-Sugar-Entry"

func encodeEntry(e vfs.Entry) string {
	b, _ := json.Marshal(e)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeEntry(s string) (vfs.Entry, error) {
	var e vfs.Entry
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &e)
	}
	return e, err
}

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

// knownCode reports whether c is one of the spec's error codes.
func knownCode(c string) bool { _, ok := statusOf[c]; return ok }
