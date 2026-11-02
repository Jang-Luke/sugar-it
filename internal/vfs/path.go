package vfs

import (
	"fmt"
	"strings"
	"time"
)

// PartDir is the per-share directory holding in-flight uploads. It is
// unreachable through the API.
const PartDir = ".sugar-it-part"

// Clean validates p and returns a slash-separated relative path; the root is "".
// Backslashes are legal filename characters and are kept, but are treated as
// separators when checking for PartDir.
func Clean(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", E(CodeInvalidPath, "path contains NUL")
	}
	if strings.HasPrefix(p, "/") {
		return "", E(CodeInvalidPath, "path must be relative")
	}
	var out []string
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			return "", E(CodeInvalidPath, "path escapes the share")
		}
		out = append(out, c)
	}
	for _, c := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(c, PartDir) {
			return "", E(CodeInvalidPath, "reserved name %s", PartDir)
		}
	}
	return strings.Join(out, "/"), nil
}

// ETag is size(hex)-mtimeNanos(hex).
func ETag(size int64, mtime time.Time) string {
	return fmt.Sprintf("%x-%x", size, mtime.UnixNano())
}
