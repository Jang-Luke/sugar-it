package identity

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

func TestLoadOrCreateStable(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("ID changed: %s vs %s", a.ID, b.ID)
	}
	if !regexp.MustCompile(`^[a-z2-7]{52}$`).MatchString(a.ID) {
		t.Fatalf("bad ID %q", a.ID)
	}
	leaf := a.Cert.Leaf
	if leaf == nil {
		t.Fatal("Leaf not set")
	}
	if IDOf(leaf) != a.ID {
		t.Fatal("IDOf mismatch")
	}
	if leaf.NotAfter.Sub(time.Now()) < 19*365*24*time.Hour {
		t.Fatalf("validity too short: %v", leaf.NotAfter)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, keyFile))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("key perm %v", fi.Mode().Perm())
		}
	}
}

func TestDistinctDirsDistinctIDs(t *testing.T) {
	a, _ := LoadOrCreate(t.TempDir())
	b, _ := LoadOrCreate(t.TempDir())
	if a.ID == b.ID {
		t.Fatal("same ID")
	}
}

func TestCorruptKeyNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, keyFile)
	os.WriteFile(p, []byte("junk"), 0o600)
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("expected error")
	}
	if b, _ := os.ReadFile(p); string(b) != "junk" {
		t.Fatal("key overwritten")
	}
}
