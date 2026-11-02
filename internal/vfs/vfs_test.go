package vfs

import (
	"errors"
	"fmt"
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
