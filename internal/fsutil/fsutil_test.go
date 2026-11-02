package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteAtomicCreatesAndReplaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := WriteAtomic(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("%q", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatal(fi.Mode())
		}
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("temp files left: %v", ents)
	}
}

func TestWriteAtomicFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	WriteAtomic(p, []byte("keep"), 0o600)
	if err := WriteAtomic(filepath.Join(dir, "missing", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("expected error")
	}
	if b, _ := os.ReadFile(p); string(b) != "keep" {
		t.Fatal("changed")
	}
}
