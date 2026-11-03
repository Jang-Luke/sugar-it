package vfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"
)

func TestClean(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""}, {".", ""}, {"a/b/", "a/b"}, {"a//b", "a/b"}, {"a/./b", "a/b"}, {`a\b`, `a\b`},
	}
	for _, c := range ok {
		got, err := Clean(c.in)
		if err != nil || got != c.want {
			t.Errorf("Clean(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	bad := []string{
		"/a", "../a", "a/../../b", "a/..", "a\x00b",
		".sugar-it-part/x", "a/.sugar-it-part",
		".SUGAR-IT-PART/x", `.sugar-it-part\x`,
	}
	for _, in := range bad {
		if got, err := Clean(in); CodeOf(err) != CodeInvalidPath {
			t.Errorf("Clean(%q) = %q, %v; want invalid_path", in, got, err)
		}
	}
}

func TestCodeOf(t *testing.T) {
	if got := CodeOf(fmt.Errorf("w: %w", E(CodeNotFound, "x"))); got != "not_found" {
		t.Errorf("got %q", got)
	}
	if got := CodeOf(errors.New("x")); got != "internal" {
		t.Errorf("got %q", got)
	}
	if got := CodeOf(nil); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestETag(t *testing.T) {
	m := time.Unix(1700000000, 5)
	if ETag(1, m) != ETag(1, m) {
		t.Error("same inputs differ")
	}
	if ETag(1, m) == ETag(2, m) {
		t.Error("size ignored")
	}
	if ETag(1, m) == ETag(1, m.Add(time.Nanosecond)) {
		t.Error("mtime ignored")
	}
}

func TestPathlessDetail(t *testing.T) {
	pe := &os.PathError{Op: "open", Path: "/secret/a", Err: fs.ErrPermission}
	le := &os.LinkError{Op: "rename", Old: "/secret/a", New: "/secret/b", Err: fs.ErrExist}
	for err, want := range map[error]string{
		pe:                            "open: permission denied",
		fmt.Errorf("saving: %w", pe):  "open: permission denied",
		le:                            "rename: file already exists",
		errors.New("no path in this"): "no path in this",
	} {
		if got := PathlessDetail(err); got != want {
			t.Errorf("%v: got %q, want %q", err, got, want)
		}
	}
}
