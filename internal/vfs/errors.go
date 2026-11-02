package vfs

import (
	"errors"
	"fmt"
)

// Error codes shared by the peer API and local callers (spec section 6).
const (
	CodeNotFound         = "not_found"
	CodeExists           = "exists"
	CodeNotEmpty         = "not_empty"
	CodeReadOnly         = "read_only"
	CodeInvalidPath      = "invalid_path"
	CodeInvalidName      = "invalid_name"
	CodeInvalidRequest   = "invalid_request"
	CodeShareNotFound    = "share_not_found"
	CodeShareOverlap     = "share_overlap"
	CodePeerNotFound     = "peer_not_found"
	CodePeerOffline      = "peer_offline"
	CodePeerUnauthorized = "peer_unauthorized"
	CodePeerIncompatible = "peer_incompatible"
	CodeChanged          = "changed"
	CodeDiskFull         = "disk_full"
	CodeHashMismatch     = "hash_mismatch"
	CodeConflict         = "conflict"
	CodeCrossHostRename  = "cross_host_rename"
	CodeCanceled         = "canceled"
	CodeInternal         = "internal"
)

// Error is a coded error whose JSON form is {"code","message"}.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// E builds an Error with a formatted message.
func E(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the code of the first *Error in err's chain: "" for nil,
// "internal" for errors that are not vfs errors.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}
